// Package config 负责 §3 的配置解析、默认值、环境变量覆盖、校验与脱敏输出。
//
// 解析规则（SPEC §3.1）：
//   - 路径来自命令行 -config，默认 ./config.json；XHX_CONFIG 可覆盖。
//   - 格式是 JSONC：允许 // 行注释、/* */ 块注释与尾逗号；逐字符扫描实现。
//   - 可选字段全部用指针，便于区分「键缺失」与「显式写了零值」。
//   - 未知键用 json.Decoder.DisallowUnknownFields() 报错。
//   - XHX_LISTEN / XHX_API_KEY / XHX_ADMIN_API_KEY 覆盖对应字段（空串视为未设置）。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// 默认值常量（集中在此，便于测试引用）。
const (
	DefaultListen                 = "0.0.0.0:3066"
	DefaultLLMBase                = "https://xiaohuanxiong.com/api/web/llm/v2"
	DefaultAuthBase               = "https://xiaohuanxiong.com/api/web/auth/v1"
	DefaultPointsBase             = "https://xiaohuanxiong.com/api/web/points/v1"
	DefaultOrigin                 = "https://xiaohuanxiong.com"
	DefaultConnectTimeout         = 30
	DefaultResponseHeaderTimeout  = 120
	DefaultRequestTimeout         = 0
	DefaultRefreshTimeout         = 30
	DefaultCatalogTimeout         = 10
	DefaultBalanceTimeout         = 20
	DefaultBillsTimeout           = 25
	DefaultUserAgent              = "xhx2api/0.1.0"
	DefaultAccountsFile           = "./data/accounts.json"
	DefaultStrategy               = "round_robin"
	DefaultRefreshMarginSeconds   = 300
	DefaultCooldownSeconds        = 600
	DefaultMaxAttempts            = 3
	DefaultMaxConcurrent          = 3
	DefaultRefreshOn401           = true
	DefaultModel                  = "sn-glm-5-3-flash"
	DefaultValidate               = "catalog"
	DefaultCatalogCacheSeconds    = 600
	DefaultCatalogCacheFile       = "./data/model_catalog.json"
	DefaultLogLevel               = "info"
	DefaultAccessLog              = true
	DefaultBufferSize             = 500
	DefaultAdminEnabled           = true
	DefaultAdminTitle             = "小浣熊反代"
	DefaultShutdownTimeoutSeconds = 10

	// §A 每日签到默认值。
	DefaultCheckinEnabled              = true
	DefaultCheckinTime                 = "00:01"
	DefaultCheckinOnStart              = true
	DefaultCheckinVerifyBills          = true
	DefaultCheckinBillsLimit           = 20
	DefaultCheckinRetryIntervalMinutes = 30
	DefaultCheckinMaxAttemptsPerDay    = 5
	DefaultCheckinHistoryFile          = "./data/checkin.json"
	DefaultCheckinHistoryDays          = 30

	// §J 网页登录默认值。
	DefaultLoginEnabled         = true
	DefaultLoginAppName         = "办公小浣熊客户端"
	DefaultLoginStateTTLSeconds = 600
)

// Config 是完整的应用配置。
type Config struct {
	Listen                 *string        `json:"listen"`
	APIKey                 *string        `json:"api_key"`
	Upstream               UpstreamConfig `json:"upstream"`
	Accounts               AccountsConfig `json:"accounts"`
	Models                 ModelsConfig   `json:"models"`
	Log                    LogConfig      `json:"log"`
	Admin                  AdminConfig    `json:"admin"`
	Checkin                CheckinConfig  `json:"checkin"`
	Login                  LoginConfig    `json:"login"`
	ShutdownTimeoutSeconds *int           `json:"shutdown_timeout_seconds"`
}

// UpstreamConfig 是上游地址与超时配置（SPEC §3.2）。
type UpstreamConfig struct {
	LLMBase                      *string           `json:"llm_base"`
	AuthBase                     *string           `json:"auth_base"`
	PointsBase                   *string           `json:"points_base"`
	Origin                       *string           `json:"origin"`
	ConnectTimeoutSeconds        *int              `json:"connect_timeout_seconds"`
	ResponseHeaderTimeoutSeconds *int              `json:"response_header_timeout_seconds"`
	RequestTimeoutSeconds        *int              `json:"request_timeout_seconds"`
	RefreshTimeoutSeconds        *int              `json:"refresh_timeout_seconds"`
	CatalogTimeoutSeconds        *int              `json:"catalog_timeout_seconds"`
	BalanceTimeoutSeconds        *int              `json:"balance_timeout_seconds"`
	BillsTimeoutSeconds          *int              `json:"bills_timeout_seconds"`
	InsecureSkipVerify           *bool             `json:"insecure_skip_verify"`
	UserAgent                    *string           `json:"user_agent"`
	ExtraHeaders                 map[string]string `json:"extra_headers"`
}

// AccountsConfig 是账号池配置（SPEC §3.2 / §5）。
type AccountsConfig struct {
	File                 *string `json:"file"`
	Strategy             *string `json:"strategy"`
	RefreshMarginSeconds *int    `json:"refresh_margin_seconds"`
	CooldownSeconds      *int    `json:"cooldown_seconds"`
	MaxAttempts          *int    `json:"max_attempts"`
	MaxConcurrent        *int    `json:"max_concurrent"`
	RefreshOn401         *bool   `json:"refresh_on_401"`
}

// ModelsConfig 是模型目录配置（SPEC §3.2 / §6.2）。
type ModelsConfig struct {
	Default             *string `json:"default"`
	Validate            *string `json:"validate"`
	CatalogCacheSeconds *int    `json:"catalog_cache_seconds"`
	CatalogCacheFile    *string `json:"catalog_cache_file"`
}

// LogConfig 是日志配置（SPEC §3.2 / §6.4）。
type LogConfig struct {
	Level      *string `json:"level"`
	AccessLog  *bool   `json:"access_log"`
	BufferSize *int    `json:"buffer_size"`
	File       *string `json:"file"`
}

// AdminConfig 是管理面板配置（SPEC §3.2 / §6.1）。
type AdminConfig struct {
	Enabled *bool   `json:"enabled"`
	APIKey  *string `json:"api_key"`
	Title   *string `json:"title"`
}

// CheckinConfig 是每日签到配置（SPEC2 §A.2）。
type CheckinConfig struct {
	Enabled              *bool   `json:"enabled"`
	Time                 *string `json:"time"`
	OnStart              *bool   `json:"on_start"`
	VerifyBills          *bool   `json:"verify_bills"`
	BillsLimit           *int    `json:"bills_limit"`
	RetryIntervalMinutes *int    `json:"retry_interval_minutes"`
	MaxAttemptsPerDay    *int    `json:"max_attempts_per_day"`
	HistoryFile          *string `json:"history_file"`
	HistoryDays          *int    `json:"history_days"`
}

// LoginConfig 是网页登录配置（SPEC5 §J）。
type LoginConfig struct {
	Enabled         *bool   `json:"enabled"`
	AppName         *string `json:"app_name"`
	StateTTLSeconds *int    `json:"state_ttl_seconds"`
}

// ResolvePath 解析配置文件路径：XHX_CONFIG 优先，其次命令行传入值。
func ResolvePath(flagPath string) string {
	if v := os.Getenv("XHX_CONFIG"); v != "" {
		return v
	}
	return flagPath
}

// Load 读取、解析、套默认值、套环境变量并校验配置。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败（%s）：%v", path, err)
	}
	cleaned, err := stripJSONC(data)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(cleaned))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败（%s）：%v", path, err)
	}
	// 拒绝 JSON 之后的多余内容（例如两个对象拼接）。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("解析配置文件失败（%s）：存在多余内容", path)
	}
	cfg.applyDefaults()
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Default 返回一份套好默认值的配置（用于测试与内部构造）。
func Default() *Config {
	var cfg Config
	cfg.applyDefaults()
	return &cfg
}

func strPtr(p *string, def string) *string {
	if p != nil {
		return p
	}
	v := def
	return &v
}

func intPtr(p *int, def int) *int {
	if p != nil {
		return p
	}
	v := def
	return &v
}

func boolPtr(p *bool, def bool) *bool {
	if p != nil {
		return p
	}
	v := def
	return &v
}

func (c *Config) applyDefaults() {
	c.Listen = strPtr(c.Listen, DefaultListen)
	c.APIKey = strPtr(c.APIKey, "")
	u := &c.Upstream
	u.LLMBase = strPtr(u.LLMBase, DefaultLLMBase)
	u.AuthBase = strPtr(u.AuthBase, DefaultAuthBase)
	u.PointsBase = strPtr(u.PointsBase, DefaultPointsBase)
	u.Origin = strPtr(u.Origin, DefaultOrigin)
	u.ConnectTimeoutSeconds = intPtr(u.ConnectTimeoutSeconds, DefaultConnectTimeout)
	u.ResponseHeaderTimeoutSeconds = intPtr(u.ResponseHeaderTimeoutSeconds, DefaultResponseHeaderTimeout)
	u.RequestTimeoutSeconds = intPtr(u.RequestTimeoutSeconds, DefaultRequestTimeout)
	u.RefreshTimeoutSeconds = intPtr(u.RefreshTimeoutSeconds, DefaultRefreshTimeout)
	u.CatalogTimeoutSeconds = intPtr(u.CatalogTimeoutSeconds, DefaultCatalogTimeout)
	u.BalanceTimeoutSeconds = intPtr(u.BalanceTimeoutSeconds, DefaultBalanceTimeout)
	u.BillsTimeoutSeconds = intPtr(u.BillsTimeoutSeconds, DefaultBillsTimeout)
	u.InsecureSkipVerify = boolPtr(u.InsecureSkipVerify, false)
	u.UserAgent = strPtr(u.UserAgent, DefaultUserAgent)
	if u.ExtraHeaders == nil {
		u.ExtraHeaders = map[string]string{}
	}
	a := &c.Accounts
	a.File = strPtr(a.File, DefaultAccountsFile)
	a.Strategy = strPtr(a.Strategy, DefaultStrategy)
	a.RefreshMarginSeconds = intPtr(a.RefreshMarginSeconds, DefaultRefreshMarginSeconds)
	a.CooldownSeconds = intPtr(a.CooldownSeconds, DefaultCooldownSeconds)
	a.MaxAttempts = intPtr(a.MaxAttempts, DefaultMaxAttempts)
	a.MaxConcurrent = intPtr(a.MaxConcurrent, DefaultMaxConcurrent)
	a.RefreshOn401 = boolPtr(a.RefreshOn401, DefaultRefreshOn401)
	m := &c.Models
	m.Default = strPtr(m.Default, DefaultModel)
	m.Validate = strPtr(m.Validate, DefaultValidate)
	m.CatalogCacheSeconds = intPtr(m.CatalogCacheSeconds, DefaultCatalogCacheSeconds)
	m.CatalogCacheFile = strPtr(m.CatalogCacheFile, DefaultCatalogCacheFile)
	l := &c.Log
	l.Level = strPtr(l.Level, DefaultLogLevel)
	l.AccessLog = boolPtr(l.AccessLog, DefaultAccessLog)
	l.BufferSize = intPtr(l.BufferSize, DefaultBufferSize)
	l.File = strPtr(l.File, "")
	ad := &c.Admin
	ad.Enabled = boolPtr(ad.Enabled, DefaultAdminEnabled)
	ad.APIKey = strPtr(ad.APIKey, "")
	ad.Title = strPtr(ad.Title, DefaultAdminTitle)
	ci := &c.Checkin
	ci.Enabled = boolPtr(ci.Enabled, DefaultCheckinEnabled)
	ci.Time = strPtr(ci.Time, DefaultCheckinTime)
	ci.OnStart = boolPtr(ci.OnStart, DefaultCheckinOnStart)
	ci.VerifyBills = boolPtr(ci.VerifyBills, DefaultCheckinVerifyBills)
	ci.BillsLimit = intPtr(ci.BillsLimit, DefaultCheckinBillsLimit)
	ci.RetryIntervalMinutes = intPtr(ci.RetryIntervalMinutes, DefaultCheckinRetryIntervalMinutes)
	ci.MaxAttemptsPerDay = intPtr(ci.MaxAttemptsPerDay, DefaultCheckinMaxAttemptsPerDay)
	ci.HistoryFile = strPtr(ci.HistoryFile, DefaultCheckinHistoryFile)
	ci.HistoryDays = intPtr(ci.HistoryDays, DefaultCheckinHistoryDays)
	lg := &c.Login
	lg.Enabled = boolPtr(lg.Enabled, DefaultLoginEnabled)
	lg.AppName = strPtr(lg.AppName, DefaultLoginAppName)
	lg.StateTTLSeconds = intPtr(lg.StateTTLSeconds, DefaultLoginStateTTLSeconds)
	c.ShutdownTimeoutSeconds = intPtr(c.ShutdownTimeoutSeconds, DefaultShutdownTimeoutSeconds)
}

func (c *Config) applyEnv() {
	if v := os.Getenv("XHX_LISTEN"); v != "" {
		c.Listen = &v
	}
	if v := os.Getenv("XHX_API_KEY"); v != "" {
		c.APIKey = &v
	}
	if v := os.Getenv("XHX_ADMIN_API_KEY"); v != "" {
		c.Admin.APIKey = &v
	}
}

// Validate 校验配置（SPEC §3.3）。任一不过返回带中文原因的 error。
func (c *Config) Validate() error {
	if _, portStr, err := net.SplitHostPort(*c.Listen); err != nil {
		return fmt.Errorf("listen 地址非法（%s）：%v", *c.Listen, err)
	} else if port, err := strconv.Atoi(portStr); err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("listen 端口非法（%s）：端口必须在 1-65535", *c.Listen)
	}
	baseChecks := []struct {
		name string
		val  string
	}{
		{"upstream.llm_base", *c.Upstream.LLMBase},
		{"upstream.auth_base", *c.Upstream.AuthBase},
		{"upstream.points_base", *c.Upstream.PointsBase},
		{"upstream.origin", *c.Upstream.Origin},
	}
	for _, check := range baseChecks {
		if !strings.HasPrefix(check.val, "http://") && !strings.HasPrefix(check.val, "https://") {
			return fmt.Errorf("%s 必须以 http:// 或 https:// 开头（当前 %q）", check.name, check.val)
		}
	}
	switch *c.Accounts.Strategy {
	case "priority", "round_robin":
	default:
		return fmt.Errorf("accounts.strategy 非法（%s）：只能是 priority 或 round_robin", *c.Accounts.Strategy)
	}
	switch *c.Models.Validate {
	case "none", "catalog":
	default:
		return fmt.Errorf("models.validate 非法（%s）：只能是 none 或 catalog", *c.Models.Validate)
	}
	switch *c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level 非法（%s）：只能是 debug/info/warn/error", *c.Log.Level)
	}
	if _, _, err := parseClock(*c.Checkin.Time); err != nil {
		return fmt.Errorf("checkin.time 非法（%s）：%v", *c.Checkin.Time, err)
	}
	seconds := []struct {
		name string
		val  int
	}{
		{"upstream.connect_timeout_seconds", *c.Upstream.ConnectTimeoutSeconds},
		{"upstream.response_header_timeout_seconds", *c.Upstream.ResponseHeaderTimeoutSeconds},
		{"upstream.request_timeout_seconds", *c.Upstream.RequestTimeoutSeconds},
		{"upstream.refresh_timeout_seconds", *c.Upstream.RefreshTimeoutSeconds},
		{"upstream.catalog_timeout_seconds", *c.Upstream.CatalogTimeoutSeconds},
		{"upstream.balance_timeout_seconds", *c.Upstream.BalanceTimeoutSeconds},
		{"upstream.bills_timeout_seconds", *c.Upstream.BillsTimeoutSeconds},
		{"accounts.refresh_margin_seconds", *c.Accounts.RefreshMarginSeconds},
		{"accounts.cooldown_seconds", *c.Accounts.CooldownSeconds},
		{"accounts.max_concurrent", *c.Accounts.MaxConcurrent},
		{"models.catalog_cache_seconds", *c.Models.CatalogCacheSeconds},
		{"log.buffer_size", *c.Log.BufferSize},
		{"checkin.retry_interval_minutes", *c.Checkin.RetryIntervalMinutes},
		{"checkin.history_days", *c.Checkin.HistoryDays},
		{"login.state_ttl_seconds", *c.Login.StateTTLSeconds},
		{"shutdown_timeout_seconds", *c.ShutdownTimeoutSeconds},
	}
	for _, s := range seconds {
		if s.val < 0 {
			return fmt.Errorf("%s 不能为负（当前 %d）", s.name, s.val)
		}
	}
	if *c.Accounts.MaxAttempts < 1 {
		return fmt.Errorf("accounts.max_attempts 必须 >= 1（当前 %d）", *c.Accounts.MaxAttempts)
	}
	if *c.Accounts.MaxConcurrent < 1 {
		return fmt.Errorf("accounts.max_concurrent 必须 >= 1（当前 %d）", *c.Accounts.MaxConcurrent)
	}
	if *c.Checkin.BillsLimit < 1 {
		return fmt.Errorf("checkin.bills_limit 必须 >= 1（当前 %d）", *c.Checkin.BillsLimit)
	}
	if *c.Checkin.MaxAttemptsPerDay < 1 {
		return fmt.Errorf("checkin.max_attempts_per_day 必须 >= 1（当前 %d）", *c.Checkin.MaxAttemptsPerDay)
	}
	if strings.TrimSpace(*c.Models.Default) == "" {
		return errors.New("models.default 不能为空")
	}
	return nil
}

// Mask 返回脱敏后的字符串：前 4 位 + "…"；长度不足 4 时返回 "***"。
func Mask(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) < 4 {
		return "***"
	}
	return string(runes[:4]) + "…"
}

// MaskedJSON 返回脱敏后的完整配置 JSON（api_key / admin.api_key 只留前 4 位）。
func (c *Config) MaskedJSON() ([]byte, error) {
	clone := *c
	key := Mask(*c.APIKey)
	clone.APIKey = &key
	adminKey := Mask(*c.Admin.APIKey)
	clone.Admin.APIKey = &adminKey
	return json.MarshalIndent(&clone, "", "  ")
}

// stripJSONC 逐字符扫描，去掉 // 行注释、/* */ 块注释与尾逗号。
//
// 关键点：字符串字面量内的 // 不能被当注释（SPEC §3.1 明确禁止用正则粗暴删除）。
func stripJSONC(data []byte) ([]byte, error) {
	var noComments []byte
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		ch := data[i]
		if inString {
			noComments = append(noComments, ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch {
		case ch == '"':
			inString = true
			noComments = append(noComments, ch)
		case ch == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				noComments = append(noComments, '\n')
			}
		case ch == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i++ // 移到 '/' 上，外层 i++ 再跳过
		default:
			noComments = append(noComments, ch)
		}
	}

	var out []byte
	inString = false
	escaped = false
	for i := 0; i < len(noComments); i++ {
		ch := noComments[i]
		if inString {
			out = append(out, ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(noComments) && (noComments[j] == ' ' || noComments[j] == '\t' || noComments[j] == '\n' || noComments[j] == '\r') {
				j++
			}
			if j < len(noComments) && (noComments[j] == '}' || noComments[j] == ']') {
				continue // 尾逗号：丢弃
			}
		}
		out = append(out, ch)
	}
	return out, nil
}

// ── 取值辅助（Load/Default 之后所有指针非 nil；这里再做一次 nil 兜底）──

// ListenAddr 返回监听地址。
func (c *Config) ListenAddr() string { return derefStr(c.Listen, DefaultListen) }

// APIKeyValue 返回 /v1 鉴权密钥。
func (c *Config) APIKeyValue() string { return derefStr(c.APIKey, "") }

// ShutdownTimeout 返回优雅退出超时。
func (c *Config) ShutdownTimeout() time.Duration {
	return time.Duration(derefInt(c.ShutdownTimeoutSeconds, DefaultShutdownTimeoutSeconds)) * time.Second
}

func derefStr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func derefBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// LLMBaseURL 返回 LLM 网关基址（末尾斜杠已去除）。
func (u *UpstreamConfig) LLMBaseURL() string {
	return strings.TrimRight(derefStr(u.LLMBase, DefaultLLMBase), "/")
}

// AuthBaseURL 返回鉴权 API 基址。
func (u *UpstreamConfig) AuthBaseURL() string {
	return strings.TrimRight(derefStr(u.AuthBase, DefaultAuthBase), "/")
}

// PointsBaseURL 返回积分 API 基址。
func (u *UpstreamConfig) PointsBaseURL() string {
	return strings.TrimRight(derefStr(u.PointsBase, DefaultPointsBase), "/")
}

// OriginURL 返回主站 origin（签到与账单接口挂在这里，末尾斜杠已去除）。
func (u *UpstreamConfig) OriginURL() string {
	return strings.TrimRight(derefStr(u.Origin, DefaultOrigin), "/")
}

// ConnectTimeout 返回 TCP 建连超时。
func (u *UpstreamConfig) ConnectTimeout() time.Duration {
	return time.Duration(derefInt(u.ConnectTimeoutSeconds, DefaultConnectTimeout)) * time.Second
}

// ResponseHeaderTimeout 返回等待响应头超时。
func (u *UpstreamConfig) ResponseHeaderTimeout() time.Duration {
	return time.Duration(derefInt(u.ResponseHeaderTimeoutSeconds, DefaultResponseHeaderTimeout)) * time.Second
}

// RequestTimeout 返回聊天请求总超时（0 = 不设）。
func (u *UpstreamConfig) RequestTimeout() time.Duration {
	return time.Duration(derefInt(u.RequestTimeoutSeconds, DefaultRequestTimeout)) * time.Second
}

// RefreshTimeout 返回 token 刷新超时。
func (u *UpstreamConfig) RefreshTimeout() time.Duration {
	return time.Duration(derefInt(u.RefreshTimeoutSeconds, DefaultRefreshTimeout)) * time.Second
}

// CatalogTimeout 返回目录请求超时。
func (u *UpstreamConfig) CatalogTimeout() time.Duration {
	return time.Duration(derefInt(u.CatalogTimeoutSeconds, DefaultCatalogTimeout)) * time.Second
}

// BalanceTimeout 返回积分请求超时。
func (u *UpstreamConfig) BalanceTimeout() time.Duration {
	return time.Duration(derefInt(u.BalanceTimeoutSeconds, DefaultBalanceTimeout)) * time.Second
}

// BillsTimeout 返回账单核对请求超时（SPEC2 §A.1）。
func (u *UpstreamConfig) BillsTimeout() time.Duration {
	return time.Duration(derefInt(u.BillsTimeoutSeconds, DefaultBillsTimeout)) * time.Second
}

// SkipVerify 返回是否跳过 TLS 校验。
func (u *UpstreamConfig) SkipVerify() bool {
	return derefBool(u.InsecureSkipVerify, false)
}

// UserAgentValue 返回出网 User-Agent。
func (u *UpstreamConfig) UserAgentValue() string { return derefStr(u.UserAgent, DefaultUserAgent) }

// AccountsFile 返回账号文件路径。
func (a *AccountsConfig) AccountsFile() string { return derefStr(a.File, DefaultAccountsFile) }

// StrategyValue 返回选号策略。
func (a *AccountsConfig) StrategyValue() string { return derefStr(a.Strategy, DefaultStrategy) }

// RefreshMargin 返回主动刷新窗口。
func (a *AccountsConfig) RefreshMargin() time.Duration {
	return time.Duration(derefInt(a.RefreshMarginSeconds, DefaultRefreshMarginSeconds)) * time.Second
}

// Cooldown 返回 429/5xx 冷却时长。
func (a *AccountsConfig) Cooldown() time.Duration {
	return time.Duration(derefInt(a.CooldownSeconds, DefaultCooldownSeconds)) * time.Second
}

// MaxAttemptsValue 返回单次请求最多尝试账号数。
func (a *AccountsConfig) MaxAttemptsValue() int { return derefInt(a.MaxAttempts, DefaultMaxAttempts) }

// MaxConcurrentValue 返回对上游的最大并发数（超出则排队）。
func (a *AccountsConfig) MaxConcurrentValue() int {
	return derefInt(a.MaxConcurrent, DefaultMaxConcurrent)
}

// RefreshOn401Value 返回 401 是否强制刷新重试。
func (a *AccountsConfig) RefreshOn401Value() bool {
	return derefBool(a.RefreshOn401, DefaultRefreshOn401)
}

// DefaultModelValue 返回默认模型。
func (m *ModelsConfig) DefaultModelValue() string { return derefStr(m.Default, DefaultModel) }

// ValidateValue 返回模型校验模式。
func (m *ModelsConfig) ValidateValue() string { return derefStr(m.Validate, DefaultValidate) }

// CatalogCacheTTL 返回目录缓存有效期。
func (m *ModelsConfig) CatalogCacheTTL() time.Duration {
	return time.Duration(derefInt(m.CatalogCacheSeconds, DefaultCatalogCacheSeconds)) * time.Second
}

// CatalogCacheFileValue 返回目录缓存文件路径。
func (m *ModelsConfig) CatalogCacheFileValue() string {
	return derefStr(m.CatalogCacheFile, DefaultCatalogCacheFile)
}

// LogLevel 返回日志级别。
func (l *LogConfig) LogLevel() string { return derefStr(l.Level, DefaultLogLevel) }

// AccessLogEnabled 返回是否打印访问日志。
func (l *LogConfig) AccessLogEnabled() bool { return derefBool(l.AccessLog, DefaultAccessLog) }

// BufferSizeValue 返回请求日志环形缓冲大小。
func (l *LogConfig) BufferSizeValue() int { return derefInt(l.BufferSize, DefaultBufferSize) }

// LogFile 返回日志文件路径（空 = 只写 stdout）。
func (l *LogConfig) LogFile() string { return derefStr(l.File, "") }

// AdminEnabled 返回是否启用管理面板。
func (a *AdminConfig) AdminEnabled() bool { return derefBool(a.Enabled, DefaultAdminEnabled) }

// AdminAPIKey 返回管理面板鉴权密钥。
func (a *AdminConfig) AdminAPIKey() string { return derefStr(a.APIKey, "") }

// AdminTitle 返回面板标题。
func (a *AdminConfig) AdminTitle() string { return derefStr(a.Title, DefaultAdminTitle) }

// parseClock 解析 HH:MM（24 小时制），返回小时与分钟（SPEC2 §A.2）。
func parseClock(s string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("格式必须为 HH:MM")
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, errors.New("小时必须是数字")
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, errors.New("分钟必须是数字")
	}
	if hour < 0 || hour > 23 {
		return 0, 0, errors.New("小时必须在 00-23 之间")
	}
	if minute < 0 || minute > 59 {
		return 0, 0, errors.New("分钟必须在 00-59 之间")
	}
	return hour, minute, nil
}

// EnabledValue 返回是否启用每日签到。
func (c *CheckinConfig) EnabledValue() bool { return derefBool(c.Enabled, DefaultCheckinEnabled) }

// TimeValue 返回每日触发时刻（HH:MM）。
func (c *CheckinConfig) TimeValue() string { return derefStr(c.Time, DefaultCheckinTime) }

// OnStartValue 返回启动时是否补签。
func (c *CheckinConfig) OnStartValue() bool { return derefBool(c.OnStart, DefaultCheckinOnStart) }

// VerifyBillsValue 返回是否以账单入账为成功判据。
func (c *CheckinConfig) VerifyBillsValue() bool {
	return derefBool(c.VerifyBills, DefaultCheckinVerifyBills)
}

// BillsLimitValue 返回账单查询条数。
func (c *CheckinConfig) BillsLimitValue() int {
	return derefInt(c.BillsLimit, DefaultCheckinBillsLimit)
}

// RetryInterval 返回当天失败后的重试间隔。
func (c *CheckinConfig) RetryInterval() time.Duration {
	return time.Duration(derefInt(c.RetryIntervalMinutes, DefaultCheckinRetryIntervalMinutes)) * time.Minute
}

// MaxAttemptsPerDayValue 返回当天最多尝试次数。
func (c *CheckinConfig) MaxAttemptsPerDayValue() int {
	return derefInt(c.MaxAttemptsPerDay, DefaultCheckinMaxAttemptsPerDay)
}

// HistoryFileValue 返回签到历史文件路径。
func (c *CheckinConfig) HistoryFileValue() string {
	return derefStr(c.HistoryFile, DefaultCheckinHistoryFile)
}

// HistoryDaysValue 返回历史保留天数。
func (c *CheckinConfig) HistoryDaysValue() int {
	return derefInt(c.HistoryDays, DefaultCheckinHistoryDays)
}

// LoginEnabled 返回是否启用网页登录（SPEC5 §J）。
func (l *LoginConfig) LoginEnabled() bool { return derefBool(l.Enabled, DefaultLoginEnabled) }

// LoginAppName 返回授权页上报的客户端名。
func (l *LoginConfig) LoginAppName() string { return derefStr(l.AppName, DefaultLoginAppName) }

// StateTTL 返回 state 有效期（SPEC5 §J）。
func (l *LoginConfig) StateTTL() time.Duration {
	return time.Duration(derefInt(l.StateTTLSeconds, DefaultLoginStateTTLSeconds)) * time.Second
}
