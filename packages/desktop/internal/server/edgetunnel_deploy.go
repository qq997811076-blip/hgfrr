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
	"net/http/httptest"
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
	AdminPassword string `json:"adminPassword,omitempty"`
	UpdatedAt string `json:"updatedAt"`
}

type edgeTunnelBinding struct {
	Project string
	Mode string
	URL string
	AdminPassword string
}

func detectEdgeTunnelBindings(ctx context.Context, client *cfapi.Client, accountID string) map[string]edgeTunnelBinding {
	found := map[string]edgeTunnelBinding{}
	// Workers：EdgeTunnel 使用名为 KV 的 KV namespace 和 ADMIN 环境变量。
	if scripts, err := client.ListScripts(ctx, accountID); err == nil {
		sub, _ := client.GetWorkersSubdomain(ctx, accountID)
		for _, name := range scripts {
			bindings, err := client.GetWorkerScriptSettings(ctx, accountID, name)
			if err != nil { continue }
			kvID, hasAdmin, adminPassword := "", false, ""
			for _, b := range bindings {
				bindingName, _ := b["name"].(string)
				bindingType, _ := b["type"].(string)
				if bindingName == "KV" && bindingType == "kv_namespace" {
					kvID, _ = b["namespace_id"].(string)
				}
				if bindingName == "ADMIN" {
					hasAdmin = true
					adminPassword, _ = b["text"].(string)
				}
			}
			if kvID != "" && hasAdmin {
				host := ""
				if sub != "" { host = "https://" + name + "." + sub + ".workers.dev" }
				found[kvID] = edgeTunnelBinding{Project:name,Mode:"workers",URL:host,AdminPassword:adminPassword}
			}
		}
	}
	// Pages：同样通过 KV / ADMIN 绑定识别，不依赖本工具写入的私有标记。
	if projects, err := client.ListPagesProjects(ctx, accountID); err == nil {
		for _, project := range projects {
			production := project.DeploymentConfigs.Production
			kv := production.KVNamespaces["KV"]
			_, hasAdmin := production.EnvVars["ADMIN"]
			if kv.NamespaceID != "" && hasAdmin {
				host := strings.TrimSpace(project.Subdomain)
				if host == "" { host = project.Name + ".pages.dev" }
				found[kv.NamespaceID] = edgeTunnelBinding{Project:project.Name,Mode:"pages",URL:"https://"+host,AdminPassword:production.EnvVars["ADMIN"].Value}
			}
		}
	}
	return found
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
	if len(raw) < 10000 || !strings.Contains(string(raw[:edgeTunnelMinInt(len(raw), 200000)]), "export default") {
		return nil, errors.New("下载到的 EdgeTunnel Worker 文件不完整或格式不正确")
	}
	return raw, nil
}

func edgeTunnelMinInt(a, b int) int { if a < b { return a }; return b }

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

	boundInstances := detectEdgeTunnelBindings(ctx, client, profile.AccountID)
	boundInstance, isBoundInstance := boundInstances[kvID]
	keys, err := client.ListKVKeys(ctx, profile.AccountID, kvID)
	if err != nil { writeJSON(w,http.StatusBadRequest,map[string]any{"error":"检查 KV 归属失败："+err.Error()}); return }
	rawManifest, state, readErr := client.GetKVValueState(ctx, profile.AccountID, kvID, edgeTunnelManifestKey)
	if readErr != nil { writeJSON(w,http.StatusBadRequest,map[string]any{"error":"读取 EdgeTunnel 部署记录失败："+readErr.Error()}); return }
	var old edgeTunnelManifest
	hasManifest := state == cfapi.KVFound && json.Unmarshal([]byte(rawManifest), &old) == nil && old.Project != ""
	if !hasManifest && !isBoundInstance && len(keys) > 0 {
		writeJSON(w,http.StatusConflict,map[string]any{"error":"所选 KV 已有其他项目的数据，未检测到 EdgeTunnel 绑定。为避免覆盖，请选择 EdgeTunnel 实例或空 KV。"})
		return
	}

	if isBoundInstance && !strings.EqualFold(req.Mode,"fresh") { mode = boundInstance.Mode }
	project := strings.TrimSpace(req.DataProject)
	if hasManifest && !strings.EqualFold(req.Mode,"fresh") { project = old.Project
	} else if isBoundInstance && !strings.EqualFold(req.Mode,"fresh") { project = boundInstance.Project }
	if project == "" { project = "edgetunnel-" + shortRandom(6) }
	admin := strings.TrimSpace(req.PanelPassword)
	if admin == "" && hasManifest && !strings.EqualFold(req.Mode,"fresh") {
		admin = old.AdminPassword
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
		if err := client.UploadWorkerScript(ctx,profile.AccountID,project,script,bindings,"2025-11-04"); err != nil {
			writeJSON(w,http.StatusBadGateway,map[string]any{"error":"上传 EdgeTunnel Worker 失败："+err.Error()}); return
		}
		sub,_ := client.GetWorkersSubdomain(ctx,profile.AccountID)
		if err := client.PutScriptSubdomain(ctx,profile.AccountID,project,true,false); err != nil {
			s.logLevel("开启 workers.dev 访问入口失败："+err.Error(),"warn")
		}
		if strings.TrimSpace(sub) == "" {
			s.logLevel("Worker 已上传，但该账号尚未设置 workers.dev 子域；请到 Cloudflare 控制台启用 workers.dev，或绑定自定义域名。","warn")
			publicURL = ""
		} else {
			publicURL = "https://"+project+"."+strings.TrimSpace(sub)+".workers.dev"
		}
	} else {
		existing, err := client.GetPagesProject(ctx,profile.AccountID,project)
		if err != nil { writeJSON(w,http.StatusBadGateway,map[string]any{"error":"查询 Pages 项目失败："+err.Error()}); return }
		envVars := map[string]cfapi.PageVar{"ADMIN":{Value:admin,Type:"plain_text"}}
		kvBindings := map[string]string{"KV":kvID}
		if existing == nil {
			if _,err := client.CreatePagesProjectConfigured(ctx,profile.AccountID,project,"main",envVars,kvBindings); err != nil {
				writeJSON(w,http.StatusBadGateway,map[string]any{"error":"创建并配置 Pages 项目失败："+err.Error()}); return
			}
			if _,err := client.DeployPagesWorker(ctx,cfapi.PagesDeployOptions{
				AccountID:profile.AccountID,Project:project,Script:script,
				CommitMessage:"Deploy CM EdgeTunnel",Branch:"main",
			}); err != nil {
				writeJSON(w,http.StatusBadGateway,map[string]any{"error":"上传 EdgeTunnel Pages 部署文件失败："+err.Error()}); return
			}
		} else {
			// 已存在的 Pages 项目必须先确认它确实绑定了当前 EdgeTunnel KV，禁止误覆盖其他项目。
			isThisInstance := (hasManifest && old.Project == project && old.Mode == "pages") ||
				(isBoundInstance && boundInstance.Mode == "pages" && boundInstance.Project == project)
			if !isThisInstance {
				writeJSON(w,http.StatusConflict,map[string]any{"error":"同名 Pages 项目已存在，但未确认属于当前 EdgeTunnel 实例；为避免覆盖其他项目，已停止部署。"})
				return
			}
			// PATCH 会触发一次部署；现有 Pages 项目已运行同一份固定版本的 _worker.js，
			// 因此只更新 ADMIN / KV 配置，不再重复提交第二次 Worker 部署。
			if err := client.ConfigurePages(ctx,profile.AccountID,project,envVars,kvBindings); err != nil {
				writeJSON(w,http.StatusBadGateway,map[string]any{"error":"更新 Pages 的 ADMIN / KV 失败："+err.Error()}); return
			}
		}
		publicURL = "https://"+project+".pages.dev"
	}

	manifest := edgeTunnelManifest{Project:project,Mode:mode,URL:publicURL,KVNamespaceID:kvID,AdminSet:true,AdminPassword:admin,UpdatedAt:time.Now().UTC().Format(time.RFC3339)}
	encoded,_ := json.Marshal(manifest)
	if err := client.SetKVValue(ctx,profile.AccountID,kvID,edgeTunnelManifestKey,string(encoded)); err != nil {
		s.logLevel("部署已上传，但写入本地实例识别标记失败："+err.Error(),"warn")
	}
	if kvTitle == "" {
		if list,e:=client.ListKVNamespaces(ctx,profile.AccountID); e==nil { for _,ns:=range list { if ns.ID==kvID { kvTitle=ns.Title; break } } }
	}
	at := time.Now().UTC().Format(time.RFC3339)
	s.rememberInstances(map[string]InstanceInfo{kvID:{
		KVTitle:kvTitle,ControlProject:"EdgeTunnel",DataProject:project,DataSubdomain:publicURL,At:at,
	}})
	s.saveSnapshot(&DeploySnapshot{
		KVNamespaceID:kvID,KVTitle:kvTitle,ControlProject:"EdgeTunnel",DataProject:project,DataSubdomain:publicURL,
		AccountID:profile.AccountID,AccountName:profile.AccountName,DataPlaneMode:mode,At:at,
	})
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

func (s *Server) startEdgeTunnelDeploy(w http.ResponseWriter, req deployRequest, profile *store.Profile) {
	s.deploy.mu.Lock()
	if s.deploy.running {
		s.deploy.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error":"已有部署正在进行，请等待完成"})
		return
	}
	s.deploy.running = true
	s.deploy.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"started":true,"note":"EdgeTunnel 部署已开始，进度会通过日志推送"})
	go func() {
		defer func() {
			s.deploy.mu.Lock()
			s.deploy.running = false
			s.deploy.mu.Unlock()
		}()
		rec := httptest.NewRecorder()
		s.handleEdgeTunnelDeploy(rec, req, profile)
		if rec.Code < 200 || rec.Code >= 300 {
			var payload map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &payload)
			msg, _ := payload["error"].(string)
			if msg == "" { msg = fmt.Sprintf("EdgeTunnel 部署失败（HTTP %d）",rec.Code) }
			s.logLevel(msg,"err")
			s.events.publish("deploy-status",map[string]any{"ok":false,"error":msg})
		}
	}()
}
