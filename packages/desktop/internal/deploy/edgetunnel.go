package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/xmwengxing/cfpilot/packages/desktop/internal/cfapi"
)

const (
	edgeTunnelWorkerURL = "https://raw.githubusercontent.com/cmliu/edgetunnel/main/_worker.js"
	edgeTunnelCompatDate = "2026-11-27"
	edgeTunnelMaxScriptBytes = 9 << 20
)

var edgeTunnelNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,57}[a-z0-9]$`)

// EdgeTunnelResult 是 EdgeTunnel 单 Worker 部署结果。
type EdgeTunnelResult struct {
	Name         string
	URL          string
	KVNamespace  string
	Admin        string
	Warnings     []string
}

// DeployEdgeTunnel 将 EdgeTunnel 官方 _worker.js 部署为单个 Cloudflare Worker。
// 只下载并部署官方核心，不改写、不裁剪其源码。
func DeployEdgeTunnel(ctx context.Context, client *cfapi.Client, accountID, name, admin, kvID string, log func(string)) (*EdgeTunnelResult, error) {
	if log == nil {
		log = func(string) {}
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = "edgetunnel-" + edgeTunnelRandomHex(4)
	}
	if !edgeTunnelNameRE.MatchString(name) {
		return nil, fmt.Errorf("Worker 名称格式无效：请使用小写字母、数字和连字符，长度为 2–59")
	}
	admin = strings.TrimSpace(admin)
	if admin == "" {
		admin = edgeTunnelRandomHex(16)
	}
	if len(admin) < 8 {
		return nil, fmt.Errorf("EdgeTunnel 管理密码至少需要 8 个字符")
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("缺少 Cloudflare Account ID")
	}
	if strings.TrimSpace(kvID) == "" {
		return nil, fmt.Errorf("缺少 EdgeTunnel 专用 KV 命名空间")
	}

	log("正在下载 EdgeTunnel 官方核心 _worker.js…")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, edgeTunnelWorkerURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hgfrr-EdgeTunnel-deployer")
	httpClient := &http.Client{Timeout: 90 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 EdgeTunnel 官方核心失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 EdgeTunnel 官方核心失败：HTTP %d", resp.StatusCode)
	}
	script, err := io.ReadAll(io.LimitReader(resp.Body, edgeTunnelMaxScriptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 EdgeTunnel 核心失败：%w", err)
	}
	if len(script) == 0 || len(script) > edgeTunnelMaxScriptBytes {
		return nil, fmt.Errorf("EdgeTunnel 核心文件大小异常：%d bytes", len(script))
	}
	if !strings.Contains(string(script[:min(len(script), 4096)]), "export default") {
		return nil, fmt.Errorf("下载到的文件不是预期的 EdgeTunnel ES Module Worker")
	}

	log(fmt.Sprintf("EdgeTunnel 官方核心已获取（%.2f MB），正在上传 Worker %s…", float64(len(script))/(1024*1024), name))
	bindings := []cfapi.WorkerBinding{
		{"type": "kv_namespace", "name": "KV", "namespace_id": strings.TrimSpace(kvID)},
		{"type": "plain_text", "name": "ADMIN", "text": admin},
	}
	if err := client.UploadWorkerScript(ctx, accountID, name, script, bindings, edgeTunnelCompatDate); err != nil {
		return nil, fmt.Errorf("上传 EdgeTunnel Worker 失败：%w", err)
	}

	warnings := []string{}
	if err := client.PutScriptSubdomain(ctx, accountID, name, true, false); err != nil {
		warnings = append(warnings, "Worker 已上传，但自动开启 workers.dev 域名失败："+err.Error())
	}
	subdomain, subErr := client.GetWorkersSubdomain(ctx, accountID)
	workerHost := name + ".workers.dev"
	if subErr == nil && strings.TrimSpace(subdomain) != "" {
		workerHost = name + "." + strings.TrimSpace(subdomain) + ".workers.dev"
	} else {
		warnings = append(warnings, "未能读取账号 workers.dev 子域；请在 Cloudflare 控制台为该 Worker 开启 workers.dev 或绑定自定义域名")
	}
	log("EdgeTunnel Worker 部署完成：https://" + workerHost)
	return &EdgeTunnelResult{
		Name: name, URL: "https://" + workerHost, KVNamespace: strings.TrimSpace(kvID),
		Admin: admin, Warnings: warnings,
	}, nil
}

func edgeTunnelRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
