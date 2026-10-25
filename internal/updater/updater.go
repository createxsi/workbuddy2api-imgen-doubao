// Package updater 版本检测与原地更新：定时查询 GitHub Release，发现新版时提醒；
// 在受支持的原生部署上可一键（或每日定时）下载、校验并替换二进制。
//
// 版本口径：Release tag 为 semver（v1.13.4），源码 appVersion 形如 1.13.4-panel；
// 比较时剥掉 v 前缀与 -panel 后缀。带预发布后缀的 tag（如 -ci 演练 tag）与
// draft/prerelease Release 一律不参与。
//
// 资产口径（与 .github/workflows/go-binaries.yml 打包一致）：
//   - linux/<arch>  : wb2api-panel-v<版本>-linux-<arch>.tar.gz，内含 wb2api
//   - windows/amd64 : wb2api-panel-doubao-windows-amd64.zip，内含 wb2api.exe
//   - darwin/<arch> : wb2api-panel-v<版本>-darwin-<arch>.tar.gz（仅提供下载链接）
//
// 校验：下载后按同一 Release 的 checksums.txt（workflow 用 sha256sum 生成）比对
// SHA-256；GitHub 资产自带 digest 时一并比对。任一不符即中止，不碰现有程序。
//
// 原地安装只在「原生 Linux/Windows amd64/arm64 + 非容器 + 未被 systemd/OpenRC/
// 服务管理器托管」时开放；其余环境只提示并给出下载链接。
package updater

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// DefaultRepository 默认查询的 GitHub 仓库。
const DefaultRepository = "createxsi/workbuddy2api-imgen-doubao"

// DefaultAutoTime 自动更新的缺省时刻（北京时间）。
const DefaultAutoTime = "04:00"

const (
	maxArchiveSize   = 300 << 20
	maxBinarySize    = 200 << 20
	maxChecksumsSize = 64 << 10
	checkEvery       = 6 * time.Hour
	userAgent        = "workbuddy2api-updater"

	selfUpdateArg = "--wb2api-self-update"
	cleanupArg    = "--wb2api-cleanup"
)

// startupProbe 新版本启动后观察多久：期间退出视为启动失败并回滚（变量便于测试缩短）。
var startupProbe = 8 * time.Second

// Settings 每日自动安装设置。
type Settings struct {
	AutoEnabled bool
	AutoTime    string // HH:MM，北京时间
}

// Status 面板可见的更新状态（经鉴权接口透出，不含敏感信息）。
type Status struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	ReleaseURL      string `json:"release_url"`
	DownloadURL     string `json:"download_url"` // 当前平台资产的下载地址（无则空）
	AssetName       string `json:"asset_name"`
	CheckedAt       string `json:"checked_at"`
	LastError       string `json:"last_error"`
	UpdateAvailable bool   `json:"update_available"`
	CanInstall      bool   `json:"can_install"`
	InstallBlocked  string `json:"install_blocked"` // 不能原地安装的原因（CanInstall=false 时）
	Installing      bool   `json:"installing"`
	AutoEnabled     bool   `json:"auto_enabled"`
	AutoTime        string `json:"auto_time"`
}

type release struct {
	TagName    string         `json:"tag_name"`
	HTMLURL    string         `json:"html_url"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

// Manager 负责版本发现、状态快照与定时安装。
type Manager struct {
	version    string
	repository string
	executable string
	apiBaseURL string
	client     *http.Client
	now        func() time.Time
	goos       string
	goarch     string
	envBlocked func() string // 环境不支持原地安装的原因；"" = 支持

	mu          sync.RWMutex
	checkMu     sync.Mutex
	settings    Settings
	status      Status
	release     *release
	lastCheck   time.Time
	lastAutoDay string
	restart     func()
}

// New 构建更新管理器。repository 为空时使用 DefaultRepository。
func New(version, repository string, settings Settings) *Manager {
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if repository == "" {
		repository = DefaultRepository
	}
	settings.AutoTime = normalizeAutoTime(settings.AutoTime)
	m := &Manager{
		version:    version,
		repository: repository,
		executable: exe,
		apiBaseURL: "https://api.github.com",
		client:     &http.Client{Timeout: 60 * time.Second},
		now:        time.Now,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		settings:   settings,
		status: Status{
			CurrentVersion: version,
			AutoEnabled:    settings.AutoEnabled,
			AutoTime:       settings.AutoTime,
		},
	}
	m.envBlocked = m.defaultEnvBlocked
	m.refreshInstallLocked()
	return m
}

// SetRestart 设置安装就绪后的重启回调（main 传入优雅停机）。
func (m *Manager) SetRestart(fn func()) {
	m.mu.Lock()
	m.restart = fn
	m.mu.Unlock()
}

// Configure 热更新自动安装设置（面板保存配置后调用）。
func (m *Manager) Configure(settings Settings) {
	settings.AutoTime = normalizeAutoTime(settings.AutoTime)
	m.mu.Lock()
	m.settings = settings
	m.status.AutoEnabled = settings.AutoEnabled
	m.status.AutoTime = settings.AutoTime
	m.mu.Unlock()
}

// Status 状态快照。
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Check 查询 Release 并选出最新的正式版本。
func (m *Manager) Check(ctx context.Context) error {
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	releases, err := m.fetchReleases(ctx)
	var newest *release
	var newestVer version
	if err == nil {
		for i := range releases {
			r := &releases[i]
			if r.Draft || r.Prerelease {
				continue
			}
			v, ok := parseVersion(r.TagName)
			if !ok || v.pre != "" {
				continue
			}
			if newest == nil || newestVer.less(v) {
				newest, newestVer = r, v
			}
		}
		if newest == nil {
			err = errors.New("没有找到可用的正式 Release")
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.status.CheckedAt = now.Format(time.RFC3339)
	m.lastCheck = now
	if err != nil {
		m.status.LastError = err.Error()
		return err
	}
	m.release = newest
	m.status.LatestVersion = newestVer.String()
	m.status.ReleaseURL = ""
	if u, perr := url.Parse(newest.HTMLURL); perr == nil && isTrustedGitHubURL(u, true) {
		m.status.ReleaseURL = newest.HTMLURL
	}
	cur, curOK := parseVersion(m.version)
	m.status.UpdateAvailable = !curOK || cur.less(newestVer)
	m.status.LastError = ""
	m.refreshInstallLocked()
	return nil
}

// refreshInstallLocked 依据环境与已知 Release 重算 CanInstall/下载链接（持 mu）。
func (m *Manager) refreshInstallLocked() {
	m.status.AssetName, m.status.DownloadURL = "", ""
	reason := m.envBlocked()
	if _, ok := parseVersion(m.version); !ok && reason == "" {
		reason = "当前版本号无法识别"
	}
	if m.release != nil {
		v, _ := parseVersion(m.release.TagName)
		name, _, _ := assetFor(v, m.goos, m.goarch)
		if a := findAsset(m.release, name); a != nil {
			m.status.AssetName = a.Name
			if u, err := url.Parse(a.BrowserDownloadURL); err == nil && isTrustedGitHubURL(u, true) {
				m.status.DownloadURL = a.BrowserDownloadURL
			}
		} else if reason == "" {
			reason = "Release 中没有当前平台（" + m.goos + "/" + m.goarch + "）的安装包"
		}
	}
	m.status.CanInstall = reason == ""
	m.status.InstallBlocked = reason
}

func (m *Manager) fetchReleases(ctx context.Context) ([]release, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=30", strings.TrimRight(m.apiBaseURL, "/"), m.repository)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 GitHub Release 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("查询 GitHub Release 失败: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 GitHub Release 失败: %w", err)
	}
	return out, nil
}

// Install 下载并校验最新 Release，暂存新程序并拉起更新进程；成功后调用方应
// 先回完响应再调 RestartAfter。任何一步失败都不改动现有程序文件。
func (m *Manager) Install(ctx context.Context) (err error) {
	m.mu.Lock()
	switch {
	case m.status.Installing:
		m.mu.Unlock()
		return errors.New("更新已在进行中")
	case m.restart == nil:
		m.mu.Unlock()
		return errors.New("更新重启回调未配置")
	}
	if reason := m.envBlocked(); reason != "" {
		m.mu.Unlock()
		return errors.New("当前部署不支持原地更新：" + reason)
	}
	m.status.Installing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if err != nil {
			m.status.Installing = false
			m.status.LastError = err.Error()
		}
		m.mu.Unlock()
	}()

	if err := m.Check(ctx); err != nil {
		return err
	}
	st := m.Status()
	if !st.UpdateAvailable {
		return errors.New("当前已是最新版本")
	}
	if !st.CanInstall {
		return errors.New("当前部署不支持原地更新：" + st.InstallBlocked)
	}
	m.mu.RLock()
	r := m.release
	m.mu.RUnlock()
	ver, _ := parseVersion(r.TagName)
	name, binName, archive := assetFor(ver, m.goos, m.goarch)
	asset := findAsset(r, name)
	if asset == nil {
		return fmt.Errorf("Release 缺少当前平台文件 %s", name)
	}
	if asset.Size <= 0 || asset.Size > maxArchiveSize {
		return fmt.Errorf("Release 文件大小异常: %d", asset.Size)
	}
	sums := findAsset(r, "checksums.txt")
	if sums == nil {
		return errors.New("Release 缺少 checksums.txt，已取消安装")
	}
	want, err := m.expectedSHA256(ctx, sums, name)
	if err != nil {
		return err
	}
	if d := strings.TrimSpace(asset.Digest); d != "" {
		if !strings.EqualFold(d, "sha256:"+want) {
			return errors.New("Release 摘要与 checksums.txt 不一致，已取消安装")
		}
	}

	dir := filepath.Dir(m.executable)
	stagePath, err := m.downloadAndStage(ctx, asset, want, dir, binName, archive)
	if err != nil {
		return err
	}
	helperPath, err := m.prepareHelper(dir)
	if err != nil {
		os.Remove(stagePath)
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return err
	}
	args, _ := json.Marshal(os.Args[1:])
	if err := launchHelper(helperPath, selfUpdateArg, strconv.Itoa(os.Getpid()), stagePath, m.executable, cwd, string(args)); err != nil {
		os.Remove(helperPath)
		os.Remove(stagePath)
		return fmt.Errorf("启动更新进程: %w", err)
	}
	log.Printf("[updater] 已准备 v%s，服务即将重启", ver.String())
	return nil
}

// downloadAndStage 下载资产、校验 SHA-256、解出程序到同目录暂存文件，返回暂存路径。
func (m *Manager) downloadAndStage(ctx context.Context, asset *releaseAsset, wantSHA, dir, binName, archive string) (string, error) {
	f, err := os.CreateTemp(dir, ".wb2api-release-*")
	if err != nil {
		return "", fmt.Errorf("创建下载临时文件: %w", err)
	}
	archivePath := f.Name()
	defer os.Remove(archivePath)
	if err := m.download(ctx, asset.BrowserDownloadURL, asset.Size, maxArchiveSize, f); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("写入下载文件: %w", err)
	}
	if err := verifySHA256(archivePath, wantSHA); err != nil {
		return "", err
	}
	sf, err := os.CreateTemp(dir, ".wb2api-stage-*"+filepath.Ext(binName))
	if err != nil {
		return "", fmt.Errorf("创建更新暂存文件: %w", err)
	}
	stagePath := sf.Name()
	sf.Close()
	if archive == "zip" {
		err = extractZip(archivePath, stagePath, binName)
	} else {
		err = extractTarGz(archivePath, stagePath, binName)
	}
	if err == nil && runtime.GOOS != "windows" {
		err = os.Chmod(stagePath, 0o755)
	}
	if err != nil {
		os.Remove(stagePath)
		return "", err
	}
	return stagePath, nil
}

// prepareHelper 把当前程序复制为更新进程：替换协议由已在运行的版本执行，
// 不依赖尚未验证能否启动的新版本。
func (m *Manager) prepareHelper(dir string) (string, error) {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	hf, err := os.CreateTemp(dir, ".wb2api-updater-*"+ext)
	if err != nil {
		return "", fmt.Errorf("创建更新进程文件: %w", err)
	}
	helperPath := hf.Name()
	hf.Close()
	if err := copyFile(m.executable, helperPath); err != nil {
		os.Remove(helperPath)
		return "", fmt.Errorf("准备更新进程: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(helperPath, 0o700); err != nil {
			os.Remove(helperPath)
			return "", err
		}
	}
	return helperPath, nil
}

// expectedSHA256 从 checksums.txt（sha256sum 输出格式）取指定文件的摘要。
func (m *Manager) expectedSHA256(ctx context.Context, sums *releaseAsset, name string) (string, error) {
	var buf strings.Builder
	if err := m.download(ctx, sums.BrowserDownloadURL, 0, maxChecksumsSize, &buf); err != nil {
		return "", fmt.Errorf("下载 checksums.txt: %w", err)
	}
	if sum, ok := parseChecksums(buf.String(), name); ok {
		return sum, nil
	}
	return "", fmt.Errorf("checksums.txt 中没有 %s 的摘要，已取消安装", name)
}

// parseChecksums 解析 sha256sum 输出（"<hex>  <name>" 或 "<hex> *<name>"）。
func parseChecksums(text, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		file := strings.TrimPrefix(fields[1], "*")
		if file != name || len(fields[0]) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			continue
		}
		return strings.ToLower(fields[0]), true
	}
	return "", false
}

// download 下载 rawURL 到 dst：只接受 github.com 起点、GitHub 官方域名重定向，
// 并限制大小（size>0 时要求精确相等）。
func (m *Manager) download(ctx context.Context, rawURL string, size, limit int64, dst io.Writer) error {
	u, err := url.Parse(rawURL)
	if err != nil || !isTrustedGitHubURL(u, true) {
		return errors.New("下载地址不是受信任的 GitHub HTTPS 地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	client := *m.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("下载重定向次数过多")
		}
		if !isTrustedGitHubURL(r.URL, false) {
			return errors.New("下载被重定向到非 GitHub 官方地址")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载更新文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载更新文件失败: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return errors.New("下载文件超过大小限制")
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("写入更新文件失败: %w", err)
	}
	if n > limit {
		return errors.New("下载文件超过大小限制")
	}
	if size > 0 && n != size {
		return fmt.Errorf("下载文件大小不符: got %d, want %d", n, size)
	}
	return nil
}

// RestartAfter 延迟触发重启回调（给 HTTP 响应留出写回时间）。
func (m *Manager) RestartAfter(delay time.Duration) {
	m.mu.RLock()
	fn := m.restart
	m.mu.RUnlock()
	if fn != nil {
		time.AfterFunc(delay, fn)
	}
}

// Run 启动即检查一次、之后每 6 小时检查；开启自动更新时每天在设定时刻
// （北京时间）检查并安装。ctx 取消即退出。
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		autoDue, regularDue := m.scheduleDue(m.now())
		if autoDue || regularDue {
			if err := m.Check(ctx); err != nil {
				if ctx.Err() == nil {
					log.Printf("WARN: [updater] 检查更新: %v", err)
				}
			} else if st := m.Status(); autoDue && st.UpdateAvailable && st.CanInstall {
				if err := m.Install(ctx); err != nil {
					log.Printf("WARN: [updater] 自动更新: %v", err)
				} else {
					m.RestartAfter(time.Second)
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) scheduleDue(now time.Time) (auto, regular bool) {
	now = now.In(cst.Loc)
	m.mu.Lock()
	defer m.mu.Unlock()
	day := now.Format("2006-01-02")
	auto = m.settings.AutoEnabled && now.Format("15:04") == m.settings.AutoTime && m.lastAutoDay != day
	if auto {
		m.lastAutoDay = day
	}
	return auto, m.lastCheck.IsZero() || now.Sub(m.lastCheck) >= checkEvery
}

// defaultEnvBlocked 判定当前进程能否原地替换自身。
func (m *Manager) defaultEnvBlocked() string {
	switch {
	case !(m.goos == "linux" || m.goos == "windows") || !(m.goarch == "amd64" || m.goarch == "arm64"):
		return "仅原生 Linux / Windows（amd64、arm64）支持一键更新"
	case m.executable == "":
		return "无法定位程序文件"
	case isContainer():
		return "容器部署，请更新镜像"
	}
	if s := supervisor(); s != "" {
		return "由 " + s + " 托管，请按原部署方式更新"
	}
	return ""
}

func normalizeAutoTime(s string) string {
	if strings.TrimSpace(s) == "" {
		return DefaultAutoTime
	}
	return s
}

// ---------------------------------------------------------------------------
// 版本

type version struct {
	major, minor, patch int
	pre                 string
}

func (v version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}

func (v version) less(o version) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	if v.patch != o.patch {
		return v.patch < o.patch
	}
	// 同核心版本：预发布 < 正式版。
	if v.pre == "" || o.pre == "" {
		return v.pre != "" && o.pre == ""
	}
	return v.pre < o.pre
}

// parseVersion 解析 v1.13.4 / 1.13.4-panel / v1.13.5-ci 等写法；-panel 是本仓
// appVersion 的固定后缀，不视为预发布。
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s = strings.TrimSuffix(s, "-panel")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 || strconv.Itoa(x) != p {
			return version{}, false
		}
		n[i] = x
	}
	return version{major: n[0], minor: n[1], patch: n[2], pre: pre}, true
}

// assetFor 当前平台在 Release 中的资产名、包内程序名与包格式。
func assetFor(v version, goos, goarch string) (name, binName, archive string) {
	if goos == "windows" {
		return "wb2api-panel-doubao-windows-" + goarch + ".zip", "wb2api.exe", "zip"
	}
	return fmt.Sprintf("wb2api-panel-v%d.%d.%d-%s-%s.tar.gz", v.major, v.minor, v.patch, goos, goarch), "wb2api", "tar.gz"
}

func findAsset(r *release, name string) *releaseAsset {
	if r == nil {
		return nil
	}
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 校验 / 解包 / 替换

func isTrustedGitHubURL(u *url.URL, initial bool) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if initial {
		return host == "github.com"
	}
	return host == "github.com" || strings.HasSuffix(host, ".github.com") ||
		host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func verifySHA256(p, want string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("更新文件 SHA-256 不匹配（got %s）", got)
	}
	return nil
}

// writeStage 把 src 写入暂存文件并检查大小。
func writeStage(stagePath string, src io.Reader, wantSize int64) error {
	if wantSize <= 0 || wantSize > maxBinarySize {
		return errors.New("更新包内的程序文件大小无效")
	}
	dst, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, io.LimitReader(src, maxBinarySize+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("解压更新文件: %w", err)
	}
	if n != wantSize {
		return errors.New("解压后的程序文件大小无效")
	}
	return nil
}

func extractZip(archivePath, stagePath, binName string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开更新压缩包失败: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if path.Clean(f.Name) != binName {
			continue
		}
		if !f.Mode().IsRegular() || f.UncompressedSize64 > maxBinarySize {
			return errors.New("更新包内的程序文件无效")
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		return writeStage(stagePath, src, int64(f.UncompressedSize64))
	}
	return fmt.Errorf("更新包中未找到 %s", binName)
}

func extractTarGz(archivePath, stagePath, binName string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("打开更新压缩包失败: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("更新包中未找到 %s", binName)
		}
		if err != nil {
			return fmt.Errorf("读取更新压缩包失败: %w", err)
		}
		if path.Clean(h.Name) != binName {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return errors.New("更新包内的程序文件无效")
		}
		return writeStage(stagePath, tr, h.Size)
	}
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// replaceWithRollback 把 stagePath 换到 targetPath，旧程序留在 targetPath+".old"；
// restore 恢复旧程序（幂等），cleanup 删除备份。
func replaceWithRollback(stagePath, targetPath string) (restore func() error, cleanup func(), err error) {
	backup := targetPath + ".old"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err := os.Rename(targetPath, backup); err != nil {
		return nil, nil, err
	}
	restored := false
	restore = func() error {
		if restored {
			return nil
		}
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(backup, targetPath); err != nil {
			return err
		}
		restored = true
		return nil
	}
	cleanup = func() { _ = os.Remove(backup) }
	if err := os.Rename(stagePath, targetPath); err != nil {
		return nil, nil, errors.Join(err, restore())
	}
	return restore, cleanup, nil
}

// ---------------------------------------------------------------------------
// 更新进程协议（main 启动最早调用 HandleSelfUpdate）

// HandleSelfUpdate 处理私有的更新/清理进程调用；普通启动返回 handled=false。
func HandleSelfUpdate(args []string) (handled bool, err error) {
	if len(args) >= 4 && args[1] == cleanupArg {
		pid, perr := strconv.Atoi(args[2])
		if perr != nil || pid <= 0 {
			return true, errors.New("清理进程参数无效")
		}
		if err := waitForProcessExit(pid, 60*time.Second); err != nil {
			return true, err
		}
		return true, os.Remove(args[3])
	}
	if len(args) < 7 || args[1] != selfUpdateArg {
		return false, nil
	}
	pid, err := strconv.Atoi(args[2])
	if err != nil || pid <= 0 {
		return true, errors.New("更新进程参数无效")
	}
	stagePath, targetPath, cwd := args[3], args[4], args[5]
	var origArgs []string
	if err := json.Unmarshal([]byte(args[6]), &origArgs); err != nil {
		return true, fmt.Errorf("解析原始启动参数: %w", err)
	}
	if err := waitForProcessExit(pid, 60*time.Second); err != nil {
		return true, err
	}
	if err := os.Chdir(cwd); err != nil {
		return true, fmt.Errorf("切换工作目录: %w", err)
	}
	helperPath, _ := os.Executable()
	defer func() {
		if runtime.GOOS == "windows" {
			launchCleanup(targetPath, helperPath, os.Getpid())
		} else {
			_ = os.Remove(helperPath)
		}
	}()
	return true, swapAndStart(stagePath, targetPath, func(probe time.Duration) error {
		return startTarget(targetPath, origArgs, probe)
	})
}

// swapAndStart 替换程序并启动新版本；替换失败或新版本启动即退出时恢复旧程序并
// 重新拉起旧版本，保证服务不因更新失败而停摆。
func swapAndStart(stagePath, targetPath string, start func(probe time.Duration) error) error {
	restore, cleanup, err := replaceWithRollback(stagePath, targetPath)
	if err != nil {
		_ = os.Remove(stagePath)
		return errors.Join(fmt.Errorf("替换程序文件: %w", err), start(0))
	}
	if err := start(startupProbe); err != nil {
		rerr := restore()
		if rerr == nil {
			rerr = start(0)
		}
		return errors.Join(fmt.Errorf("启动新版本: %w", err), rerr)
	}
	cleanup()
	return nil
}

// startTarget 拉起程序；probe>0 时观察这么久，期间退出视为启动失败。
func startTarget(targetPath string, args []string, probe time.Duration) error {
	cmd := exec.Command(targetPath, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Start(); err != nil {
		return err
	}
	if probe <= 0 {
		return cmd.Process.Release()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			err = errors.New("进程启动后立即退出")
		}
		return err
	case <-time.After(probe):
		return nil // 子进程继续运行；更新进程退出后由系统收养
	}
}

func launchHelper(helperPath string, args ...string) error {
	cmd := exec.Command(helperPath, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	setHiddenWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
