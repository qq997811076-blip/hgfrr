package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xmwengxing/cfpilot/packages/desktop/internal/cfapi"
	"github.com/xmwengxing/cfpilot/packages/desktop/internal/store"
)

const (
	edgeTunnelManifestKey = "EDGETUNNEL_DEPLOY_MANIFEST"
	edgeTunnelSourceURL = "https://raw.githubusercontent.com/cmliu/edgetunnel/b5bcc06467438f99467e9a8b16e6cd3fbfeed4d1/_worker.js"
)

type edgeTunnelManifest struct {
	Project string `json:"project"`
	Mode string `json:"mode"`
	URL string `json:"url"`
	KVNamespaceID string `json:"kvNamespaceId"`
	AdminSet bool `json:"adminSet"`
	UpdatedAt string `json:"updatedAt"`
}

func edgeTunnelRandomPassword() string {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil { return fmt.Sprintf("EDT-%d", time.Now().UnixNano()) }
	return hex.EncodeToString(b[:])
}

func loadEdgeTunnelWorker(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, edgeTunnelSourceURL, nil)
	if err != nil { return nil, err }
	req.Header.Set("User-Agent", "CFPilot-EdgeTunnel-Deploy")
	client := &http.Client{Timeout: 90 * time.Second}
	res, err := client.Do(req)
	if err != nil { return nil, fmt.Errorf("下载 EdgeTunnel _worker.js 失败：%w", err) }
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK { return nil, fmt.Errorf("下载 EdgeTunnel _worker.js 失败：HTTP %d", res.StatusCode) }
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil { return nil, err }
	if len(raw) < 10000 || !strings.Contains(string(raw[:minInt(len(raw), 200000)]), "export default") {
		return nil, errors.New("下载到的 EdgeTunnel Worker 文件不完整或格式不正确")
	}
	return raw, nil
}

func minInt(a, b int) int { if a < b { return a }; return b }

func (s *Server) handleEdgeTunnelDeploy(w http.ResponseWriter, req deployRequest, profile *store.Profile) {
	mode := strings.ToLower(strings.TrimSpace(req.DataPlaneMode))
	if mode != "workers" { mode = "pages" }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := s.newCFClient(profile.CfToken)
	kvID := strings.TrimSpace(req.KVNamespaceID)
	kvTitle := ""
	if strings.EqualFold(req.Mode, "fresh") {
		title := "edgetunnel-kv-" + shortRandom(8)
		ns, err := client.CreateKVNamespace(ctx, profile.AccountID, title)
		if err != nil { writeJSON(w, http.StatusBadRequest, map[string]any{"error":"创建 EdgeTunnel KV 失败："+err.Error()}); return }
		kvID, kvTitle = ns.ID, ns.Title
		if _, err := s.store.SaveActive(store.Patch{KVNamespaceID:&kvID}); err != nil { s.logLevel("保存 KV 选择失败："+err.Error(),"warn") }
	} else if kvID == "" {
		kvID = strings.TrimSpace(profile.KVNamespaceID)
	}
	if kvID == "" { writeJSON(w,http.StatusBadRequest,map[string]any{"error":"请先选择 KV 命名空间，或选择全新部署"}); return }

	keys, err := client.ListKVKeys(ctx, profile.AccountID, kvID)
	if err != nil { writeJSON(w,http.StatusBadRequest,map[string]any{"error":"检查 KV 归属失败："+err.Error()}); return }
	rawManifest, state, readErr := client.GetKVValueState(ctx, profile.AccountID, kvID, edgeTunnelManifestKey)
	if readErr != nil { writeJSON(w,http.StatusBadRequest,map[string]any{"error":"读取 EdgeTunnel 部署记录失败："+readErr.Error()}); return }
	var old edgeTunnelManifest
	hasManifest := state == cfapi.KVFound && json.Unmarshal([]byte(rawManifest), &old) == nil && old.Project != ""
	if !hasManifest && len(keys) > 0 {
		writeJSON(w,http.StatusConflict,map[string]any{"error":"所选 KV 已有其他项目的数据，未检测到 EdgeTunnel 部署记录。为避免覆盖，请选择空 KV 或全新部署。"})
		return
	}

	project := strings.TrimSpace(req.DataProject)
	if hasManifest && !strings.EqualFold(req.Mode,"fresh") { project = old.Project }
	if project == "" { project = "edgetunnel-" + shortRandom(6) }
	admin := strings.TrimSpace(req.PanelPassword)
	if admin == "" && hasManifest && !strings.EqualFold(req.Mode,"fresh") {
		// EdgeTunnel ADMIN is a Cloudflare binding and cannot be read back reliably.
		// Keep the saved local deployment password if the caller supplied it; otherwise generate a new one.
	}
	if admin == "" { admin = edgeTunnelRandomPassword() }

	s.logLevel("正在获取 CM EdgeTunnel 官方 _worker.js…","info")
	script, err := loadEdgeTunnelWorker(ctx)
	if err != nil { writeJSON(w,http.StatusBadGateway,map[string]any{"error":err.Error()}); return }
	s.logLevel(fmt.Sprintf("已获取 EdgeTunnel Worker（%.1f KB）",float64(len(script))/1024),"ok")

	var publicURL string
	if mode == "workers" {
		bindings := []cfapi.WorkerBinding{
			{"type":"kv_namespace","name":"KV","namespace_id":kvID},
			{"type":"plain_text","name":"ADMIN","text":admin},
		}
		s.logLevel("正在上传 EdgeTunnel Worker…","info")
		if err := client.UploadWorkerScript(ctx,profile.AccountID,project,script,bindings,"2026-11-01"); err != nil {
			writeJSON(w,http.StatusBadGateway,map[string]any{"error":"上传 EdgeTunnel Worker 失败："+err.Error()}); return
		}
		sub,_ := client.GetWorkersSubdomain(ctx,profile.AccountID)
		if err := client.PutScriptSubdomain(ctx,profile.AccountID,project,true,false); err != nil {
			s.logLevel("开启 workers.dev 访问入口失败："+err.Error(),"warn")
		}
		publicURL = "https://"+project+"."+strings.TrimSpace(sub)+".workers.dev"
		if strings.TrimSpace(sub)=="" { publicURL = "https://"+project+".workers.dev" }
	} else {
		existing, err := client.GetPagesProject(ctx,profile.AccountID,project)
		if err != nil { writeJSON(w,http.StatusBadGateway,map[string]any{"error":"查询 Pages 项目失败："+err.Error()}); return }
		if existing == nil {
			if _,err := client.CreatePagesProject(ctx,profile.AccountID,project,"main"); err != nil {
				writeJSON(w,http.StatusBadGateway,map[string]any{"error":"创建 Pages 项目失败："+err.Error()}); return
			}
		}
		// Pages 项目配置与 Worker 文件使用同一份 EdgeTunnel 源码。
		if err := client.ConfigurePages(ctx,profile.AccountID,project,
			map[string]cfapi.PageVar{"ADMIN":{Value:admin,Type:"plain_text"}},
			map[string]string{"KV":kvID}); err != nil {
			writeJSON(w,http.StatusBadGateway,map[string]any{"error":"配置 Pages 的 ADMIN / KV 失败："+err.Error()}); return
		}
		if _,err := client.DeployPagesWorker(ctx,cfapi.PagesDeployOptions{
			AccountID:profile.AccountID,Project:project,Script:script,
			CommitMessage:"Deploy CM EdgeTunnel",Branch:"main",
		}); err != nil {
			writeJSON(w,http.StatusBadGateway,map[string]any{"error":"上传 EdgeTunnel Pages 部署文件失败："+err.Error()}); return
		}
		publicURL = "https://"+project+".pages.dev"
	}

	manifest := edgeTunnelManifest{Project:project,Mode:mode,URL:publicURL,KVNamespaceID:kvID,AdminSet:true,UpdatedAt:time.Now().UTC().Format(time.RFC3339)}
	encoded,_ := json.Marshal(manifest)
	if err := client.SetKVValue(ctx,profile.AccountID,kvID,edgeTunnelManifestKey,string(encoded)); err != nil {
		s.logLevel("部署已上传，但写入本地实例识别标记失败："+err.Error(),"warn")
	}
	if kvTitle == "" {
		if list,e:=client.ListKVNamespaces(ctx,profile.AccountID); e==nil { for _,ns:=range list { if ns.ID==kvID { kvTitle=ns.Title; break } } }
	}
	s.logLevel("EdgeTunnel 部署完成："+publicURL,"ok")
	s.logLevel("EdgeTunnel 管理密码（ADMIN）："+admin,"ok")
	result := map[string]any{
		"control":map[string]any{"name":"EdgeTunnel","subdomain":""},
		"data":map[string]any{"name":project,"subdomain":publicURL},
		"dataPlaneMode":mode,"panelPath":"/admin","panelPassword":admin,
		"kvNamespaceId":kvID,"kvTitle":kvTitle,"url":publicURL,
	}
	s.events.publish("deploy-status",map[string]any{"ok":true,"result":result})
}
