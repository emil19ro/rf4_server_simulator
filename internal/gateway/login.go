package gateway

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"rf4server/internal/config"
	"rf4server/internal/metrics"
	"rf4server/internal/portal"
	"rf4server/internal/proto"
)

// 指标（NewLoginServer 里注册；nil 时为 no-op 计数器）。
var (
	loginRequests prometheus.Counter
	loginIssued   prometheus.Counter
	loginErrors   prometheus.Counter
)

// GreetingUUID 客户端校验的服务端问候（协议常量，实现在 proto.GreetingUUID）。
const GreetingUUID = proto.GreetingUUID

const loginXMLTpl = `<logon success="true"><host>%s</host><port>%d</port>` +
	`<region>%s</region><server>%s</server><userid>%d</userid>` +
	`<token>%s</token><logintoken>%s</logintoken>` +
	`<logintokenexpires>%d</logintokenexpires></logon>`

// legacyLoginXMLTpl 是旧版 CN 客户端在 7515 明文兼容口上要求的响应格式。
// 客户端按 <response> 根节点解析；返回 <logon> 会报 Invalid URI。
const legacyLoginXMLTpl = `<?xml version="1.0" encoding="UTF-8"?>
<response success="true"><token>%s</token><host>%s</host><port>%d</port>` +
	`<region>%s</region><userid>%d</userid><logintoken>%s</logintoken>` +
	`<logintokenexpires>%d</logintokenexpires></response>`

// LoginServer HTTPS 登录服务：任何路径都返回 <logon> XML（客户端打 /login.php 或 /steam.php）。
type LoginServer struct {
	cfg       config.Gateway
	store     Store
	log       *slog.Logger
	handler   http.Handler
	srv       *http.Server
	ln        net.Listener
	compatSrv *http.Server
	compatLn  net.Listener
	userid    int64 // 本进程内递增序号，共享序号不可用时使用
	ipUID     *ipUIDTable
}

// NewLoginServer 构建登录服务（含观测指标注册）。
func NewLoginServer(cfg config.Gateway, store Store, log *slog.Logger, obs *metrics.Server) (*LoginServer, error) {
	s := &LoginServer{cfg: cfg, store: store, log: log, ipUID: newIPUIDTable(cfg)}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("加载 TLS 证书: %w", err)
	}
	// RF4 CN 客户端会把上游 URL 原样编码进路径，例如：
	// /lv54/https://api.rf4game.ru/login.php。http.ServeMux 会把其中的
	// 双斜杠清理为单斜杠并返回 307，客户端随后会因 URI 被破坏而登录失败。
	// 登录服务本来就接受任意路径，因此直接使用 HandlerFunc，保留 RequestURI。
	s.handler = http.HandlerFunc(s.handleLogin)
	if storage, ok := store.(*SQLiteStore); ok {
		accountPortal := portal.New(storage.DB, s.useridForRemote)
		s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/portal/") {
				accountPortal.ServeHTTP(w, r)
				return
			}
			s.handleLogin(w, r)
		})
	}
	s.srv = &http.Server{
		Addr:              cfg.LoginAddr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
	}
	if cfg.LoginCompatAddr != "" {
		s.compatSrv = &http.Server{
			Addr:              cfg.LoginCompatAddr,
			Handler:           s.handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	// 指标（无观测端口时用本地一次性注册表，避免 nil 指标 panic）
	if obs != nil {
		reg := obs.Registry()
		loginRequests = metrics.NewCounter(reg, "gateway_login_requests_total", "登录请求数")
		loginIssued = metrics.NewCounter(reg, "gateway_tokens_issued_total", "签发 token 数")
		loginErrors = metrics.NewCounter(reg, "gateway_login_errors_total", "登录处理错误数")
	} else {
		reg := prometheus.NewRegistry()
		loginRequests = metrics.NewCounter(reg, "gateway_login_requests_total", "登录请求数")
		loginIssued = metrics.NewCounter(reg, "gateway_tokens_issued_total", "签发 token 数")
		loginErrors = metrics.NewCounter(reg, "gateway_login_errors_total", "登录处理错误数")
	}
	return s, nil
}

// Start 创建 TLS listener 并开始服务；返回错误 channel 与绑定错误。
func (s *LoginServer) Start() (<-chan error, error) {
	ln, err := net.Listen("tcp", s.cfg.LoginAddr)
	if err != nil {
		return nil, fmt.Errorf("登录服务监听 %s: %w", s.cfg.LoginAddr, err)
	}
	s.ln = tls.NewListener(ln, s.srv.TLSConfig)
	errCh := make(chan error, 2)
	go func() {
		if err := s.srv.Serve(s.ln); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("登录服务 %s: %w", s.cfg.LoginAddr, err)
		}
	}()
	if s.compatSrv != nil {
		compatLn, err := net.Listen("tcp", s.cfg.LoginCompatAddr)
		if err != nil {
			_ = s.ln.Close()
			return nil, fmt.Errorf("登录兼容口监听 %s: %w", s.cfg.LoginCompatAddr, err)
		}
		s.compatLn = compatLn
		go func() {
			if err := s.compatSrv.Serve(s.compatLn); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("登录兼容口 %s: %w", s.cfg.LoginCompatAddr, err)
			}
		}()
	}
	return errCh, nil
}

// Addr 实际监听地址（Start 之后有效）。
func (s *LoginServer) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// CompatAddr 明文 HTTP 兼容口的实际监听地址；未启用时为空。
func (s *LoginServer) CompatAddr() string {
	if s.compatLn == nil {
		return ""
	}
	return s.compatLn.Addr().String()
}

// Shutdown 优雅停机。
func (s *LoginServer) Shutdown(ctx context.Context) error {
	var first error
	if s.compatSrv != nil {
		first = s.compatSrv.Shutdown(ctx)
	}
	if err := s.srv.Shutdown(ctx); first == nil {
		first = err
	}
	return first
}

func (s *LoginServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	loginRequests.Inc()
	_ = r.Body.Close()
	// 客户端登录不发送账号凭据；本机固定 userid，其他来源 IP 分配独立 userid，
	// 这样同一台机器重登档案连续，Radmin/局域网多客户端也能各自有账号。
	userid := s.useridForRemote(r.RemoteAddr)
	if userid <= 0 {
		userid, _ = s.nextUserid(r.Context())
	}
	info := TokenInfo{Userid: userid, IssuedAt: time.Now().Unix(), Region: s.cfg.Region}
	ttl := time.Duration(s.cfg.TokenTTL) * 24 * time.Hour
	expires := time.Now().Add(ttl).Unix()
	var token, logintoken, xml string
	if r.TLS == nil {
		// 旧版 CN 客户端会把 token 当 URI 片段使用，不能包含 '|' 或 '/'。
		// 这里沿用旧后端的 32hex token / 48hex logintoken，并登记到同一 SQLite。
		token = NewLegacyToken()
		logintoken = NewLegacyLoginToken()
		if err := s.store.SaveToken(r.Context(), token, info, ttl); err != nil {
			loginErrors.Inc()
			s.log.Error("保存兼容 token 失败", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.store.SaveToken(r.Context(), logintoken, info, ttl); err != nil {
			loginErrors.Inc()
			s.log.Error("保存兼容登录 token 失败", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		xml = fmt.Sprintf(legacyLoginXMLTpl,
			token, s.cfg.AdvertiseHost, s.cfg.AdvertisePort,
			s.cfg.Region, userid, logintoken, expires)
	} else {
		token = NewToken(userid)
		if err := s.store.SaveToken(r.Context(), token, info, ttl); err != nil {
			loginErrors.Inc()
			s.log.Error("保存 token 失败", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// HTTPS 作者通道继续使用 4 段 logintoken。realtime 认证只认 gw_token 里的
		// 签发记录，因此它必须和 token 一样登记，否则客户端拿它接 realtime 会被拒。
		logintoken = NewLogintoken(userid)
		if err := s.store.SaveToken(r.Context(), logintoken, info, ttl); err != nil {
			loginErrors.Inc()
			s.log.Error("保存 logintoken 失败", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		xml = fmt.Sprintf(loginXMLTpl,
			s.cfg.AdvertiseHost, s.cfg.AdvertisePort,
			s.cfg.Region, s.cfg.ServerName, userid,
			token, logintoken, expires)
	}
	loginIssued.Inc()
	if r.TLS == nil {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(xml)))
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml))
	s.log.Info("登录",
		"method", r.Method, "request_uri", r.RequestURI, "path", r.URL.Path,
		"raw_path", r.URL.RawPath, "remote", r.RemoteAddr,
		"userid", userid, "region", s.cfg.Region, "realtime", s.cfg.AdvertiseHost)
}

func (s *LoginServer) nextUserid(ctx context.Context) (int64, error) {
	// 从共享存储获取下一个 userid；共享存储不可用时退化为进程内序号。
	if v, err := s.store.NextUserid(ctx); err == nil {
		return v, nil
	} else {
		s.log.Warn("共享 userid 序号不可用，退化进程内序号", "err", err)
	}
	return atomic.AddInt64(&s.userid, 1), nil
}

// NewToken 生成 4 段 token：hex(32B)|unix|userid|base64(168B)。
// 整串是 RC4 密钥，客户端会在 realtime 握手时回传。
func NewToken(userid int64) string {
	var h [32]byte
	_, _ = rand.Read(h[:])
	var b [168]byte
	_, _ = rand.Read(b[:])
	return strings.Join([]string{
		hex.EncodeToString(h[:]),
		strconv.FormatInt(time.Now().Unix(), 10),
		strconv.FormatInt(userid, 10),
		base64.StdEncoding.EncodeToString(b[:]),
	}, "|")
}

// NewLegacyToken 生成旧版 CN 客户端使用的 32 位十六进制 token。
func NewLegacyToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewLegacyLoginToken 生成旧版 CN 客户端使用的 48 位十六进制 logintoken。
func NewLegacyLoginToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewLogintoken 生成登录 XML 使用的短 token，供辅助 realtime 通道认证。
// 该 token 同时作为 RC4 密钥；第三段是玩家 uid。
func NewLogintoken(userid int64) string {
	var s [6]byte
	_, _ = rand.Read(s[:])
	var t [8]byte
	_, _ = rand.Read(t[:])
	return strings.Join([]string{
		"local",
		hex.EncodeToString(s[:]),
		strconv.FormatInt(userid, 10),
		hex.EncodeToString(t[:]),
	}, "|")
}

// ---- 局域网/Radmin 多账号：按来源 IP 分配 userid ----

func (s *LoginServer) useridForRemote(remote string) int64 {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	base := s.cfg.UseridStart
	if base <= 0 {
		base = 1000001
	}
	// 本机保持原固定 UID，避免现有存档换号。
	if host == "" || host == "127.0.0.1" || host == "::1" || host == "localhost" {
		return base
	}
	return s.ipUID.uid(host, base, s.log)
}

// defaultIPUIDFile 是 IP→userid 映射表的默认文件名，相对进程工作目录解析。
// 表记录的是玩家身份，换工作目录等于换一张表，部署时可用 gateway.userid_map_file 固定。
const defaultIPUIDFile = "gateway_ip_uids.json"

// ipUIDTable 按来源 IP 分配 userid：首次分配时读盘，之后常驻内存，
// 每次新增条目都原子写回磁盘——分配结果落不了盘，重启后同一 IP 就会换号。
// 读不明白时禁止写盘：磁盘上的旧表可能完好，写回等于把它覆盖成单行。
type ipUIDTable struct {
	mu     sync.Mutex
	path   string
	byHost map[string]int64
	loaded bool
	// persistable 表示允许写盘：只有「文件不存在（首次运行）」或「读表成功」才置真。
	persistable bool
	// saveBlockedLogged 保证「本次运行不落盘」只记一条 Error，不随每个新 IP 刷屏。
	saveBlockedLogged bool
	// readFile/renameFile 默认用 os.ReadFile/os.Rename，测试注入以模拟读失败与备份失败。
	readFile   func(name string) ([]byte, error)
	renameFile func(oldpath, newpath string) error
}

// newIPUIDTable 按配置解析映射表路径；未配置时用工作目录下的默认文件名。
func newIPUIDTable(cfg config.Gateway) *ipUIDTable {
	path := cfg.UseridMapFile
	if path == "" {
		path = defaultIPUIDFile
	}
	return &ipUIDTable{path: path, byHost: map[string]int64{}, readFile: os.ReadFile, renameFile: os.Rename}
}

// uid 返回 host 的 userid；没有条目时取表中最大值 +1（不低于 base），并写回磁盘。
func (t *ipUIDTable) uid(host string, base int64, log *slog.Logger) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked(log)
	if uid, ok := t.byHost[host]; ok && uid > 0 {
		return uid
	}
	max := base
	for _, v := range t.byHost {
		if v > max {
			max = v
		}
	}
	uid := max + 1
	t.byHost[host] = uid
	// 写盘失败不回滚内存：本次登录已按这个 userid 发 token，回滚会让同一进程内前后换号。
	t.saveLocked(log)
	log.Info("LAN 账号分配", "ip", host, "userid", uid)
	return uid
}

// loadLocked 只在首次分配时读一次盘，并决定本次运行能否写盘。
// 只有「文件不存在（首次运行）」和「读表成功」允许写；读不出时磁盘上的表可能完好，
// 若按空表发号后无条件写回，整张旧表会被覆盖成单行，其它 IP 的历史 userid 全部丢失。
func (t *ipUIDTable) loadLocked(log *slog.Logger) {
	if t.loaded {
		return
	}
	t.loaded = true
	readFile := t.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	b, err := readFile(t.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：没有旧表可丢，正常建表。
			t.persistable = true
			return
		}
		log.Error("读取 LAN userid 映射表失败，按空表发号且本次运行不写盘",
			"path", t.path, "err", err)
		return
	}
	var m map[string]int64
	if err := json.Unmarshal(b, &m); err != nil {
		log.Error("LAN userid 映射表损坏，按空表发号", "path", t.path, "err", err)
		t.persistable = t.backupCorruptLocked(log)
		return
	}
	if m != nil {
		t.byHost = m
	}
	t.persistable = true
}

// backupCorruptLocked 把损坏的表改名留证，只有改名成功才允许以空表重建；
// 改名失败说明坏文件还占着位，此时写盘等于覆盖唯一的历史线索，必须继续禁止写。
func (t *ipUIDTable) backupCorruptLocked(log *slog.Logger) bool {
	renameFile := t.renameFile
	if renameFile == nil {
		renameFile = os.Rename
	}
	backup := t.path + ".corrupt-" + strconv.FormatInt(time.Now().Unix(), 10)
	// 同一秒内重复备份（崩溃循环重启）时补纳秒后缀：os.Rename 会覆盖已存在的目标，
	// 覆盖上一份备份等于销毁唯一的历史线索。
	if _, err := os.Stat(backup); err == nil {
		backup += "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if err := renameFile(t.path, backup); err != nil {
		log.Error("备份损坏的 LAN userid 映射表失败，本次运行不写盘",
			"path", t.path, "backup", backup, "err", err)
		return false
	}
	log.Error("损坏的 LAN userid 映射表已改名备份，将以空表重建",
		"path", t.path, "backup", backup)
	return true
}

// saveLocked 把整张表原子写回；失败只记录，不影响已经分配的 userid。
// 读表没成功时跳过写盘：此时内存表不代表磁盘旧表，写回就是把它覆盖成单行。
func (t *ipUIDTable) saveLocked(log *slog.Logger) {
	if !t.persistable {
		if !t.saveBlockedLogged {
			t.saveBlockedLogged = true
			log.Error("LAN userid 映射表本次运行不会落盘，重启后这些 IP 可能换号",
				"path", t.path)
		}
		return
	}
	b, err := json.MarshalIndent(t.byHost, "", "  ")
	if err != nil {
		log.Error("序列化 LAN userid 映射表失败", "path", t.path, "err", err)
		return
	}
	if err := writeFileAtomic(t.path, b, 0o644); err != nil {
		log.Error("写入 LAN userid 映射表失败，重启后该 IP 可能换号", "path", t.path, "err", err)
	}
}

// writeFileAtomic 先在同目录写临时文件并 Sync，再 rename 覆盖目标文件。
// 直接覆盖时进程若在写入中途退出会留下截断 JSON，下次启动整张表被判坏清零。
// Windows 上 os.Rename 走 MoveFileEx(MOVEFILE_REPLACE_EXISTING)，可覆盖已存在文件；
// 目标被其他进程独占打开时 rename 会失败，此时保留旧文件并返回错误，由调用方记录。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// rename 成功后 tmp 已不存在；保留兜底清理，避免失败路径留下临时文件。
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
