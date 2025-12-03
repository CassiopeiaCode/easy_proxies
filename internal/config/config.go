package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config describes the high level settings for the proxy pool server.
type Config struct {
	Mode                        string           `yaml:"mode"`
	Listener                    ListenerConfig   `yaml:"listener"`
	MultiPort                   MultiPortConfig  `yaml:"multi_port"`
	Pool                        PoolConfig       `yaml:"pool"`
	Management                  ManagementConfig `yaml:"management"`
	Nodes                       []NodeConfig     `yaml:"nodes"`
	NodesFile                   string           `yaml:"nodes_file"`    // 节点文件路径，每行一个 URI
	Subscriptions               []string         `yaml:"subscriptions"` // 订阅链接列表
	ExternalIP                  string           `yaml:"external_ip"`   // 外部 IP 地址，用于导出时替换 0.0.0.0
	LogLevel                    string           `yaml:"log_level"`
	StateFile                   string           `yaml:"state_file"`                    // 节点状态持久化文件
	StateFlushInterval          time.Duration    `yaml:"state_flush_interval"`          // 状态刷盘周期
	SubscriptionRefreshInterval time.Duration    `yaml:"subscription_refresh_interval"` // 订阅自动刷新周期
	ConfigPath                  string           `yaml:"-"`
}

// ListenerConfig defines how the HTTP proxy should listen for clients.
type ListenerConfig struct {
	Address  string `yaml:"address"`
	Port     uint16 `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// PoolConfig configures scheduling + failure handling.
type PoolConfig struct {
	Mode              string        `yaml:"mode"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	BlacklistDuration time.Duration `yaml:"blacklist_duration"`
}

// MultiPortConfig defines address/credential defaults for multi-port mode.
type MultiPortConfig struct {
	Address  string `yaml:"address"`
	BasePort uint16 `yaml:"base_port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// ManagementConfig controls the monitoring HTTP endpoint.
type ManagementConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	Listen      string `yaml:"listen"`
	ProbeTarget string `yaml:"probe_target"`
	Password    string `yaml:"password"` // WebUI 访问密码，为空则不需要密码
}

// NodeConfig describes a single upstream proxy endpoint expressed as URI.
type NodeConfig struct {
	Name           string `yaml:"name"`
	URI            string `yaml:"uri"`
	Port           uint16 `yaml:"port,omitempty"`
	Username       string `yaml:"username,omitempty"`
	Password       string `yaml:"password,omitempty"`
	EndpointID     string `yaml:"-"`
	EndpointHost   string `yaml:"-"`
	EndpointPort   uint16 `yaml:"-"`
	EndpointScheme string `yaml:"-"`
}

type endpointMeta struct {
	host   string
	port   uint16
	scheme string
}

var defaultEndpointPorts = map[string]uint16{
	"vmess":     443,
	"vless":     443,
	"trojan":    443,
	"ss":        8388,
	"ssr":       8388,
	"hysteria":  443,
	"hysteria2": 443,
	"hy2":       443,
}

// Load reads YAML config from disk and applies defaults/validation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.ConfigPath = path
	configDir := filepath.Dir(path)

	// Resolve nodes_file path relative to config file directory
	if cfg.NodesFile != "" && !filepath.IsAbs(cfg.NodesFile) {
		cfg.NodesFile = filepath.Join(configDir, cfg.NodesFile)
	}
	if cfg.StateFile != "" && !filepath.IsAbs(cfg.StateFile) {
		cfg.StateFile = filepath.Join(configDir, cfg.StateFile)
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	if c.Mode == "" {
		c.Mode = "pool"
	}
	// Normalize mode name: support both multi-port and multi_port
	if c.Mode == "multi_port" {
		c.Mode = "multi-port"
	}
	switch c.Mode {
	case "pool", "multi-port":
	default:
		return fmt.Errorf("unsupported mode %q (use 'pool' or 'multi-port')", c.Mode)
	}
	if c.Listener.Address == "" {
		c.Listener.Address = "0.0.0.0"
	}
	if c.Listener.Port == 0 {
		c.Listener.Port = 2323
	}
	if c.Pool.Mode == "" {
		c.Pool.Mode = "sequential"
	}
	if c.Pool.FailureThreshold <= 0 {
		c.Pool.FailureThreshold = 3
	}
	if c.Pool.BlacklistDuration <= 0 {
		c.Pool.BlacklistDuration = 24 * time.Hour
	}
	if c.MultiPort.Address == "" {
		c.MultiPort.Address = "0.0.0.0"
	}
	if c.MultiPort.BasePort == 0 {
		c.MultiPort.BasePort = 28000
	}
	if c.Management.Listen == "" {
		c.Management.Listen = "127.0.0.1:9090"
	}
	if c.Management.ProbeTarget == "" {
		c.Management.ProbeTarget = "www.apple.com:80"
	}
	if c.Management.Enabled == nil {
		defaultEnabled := true
		c.Management.Enabled = &defaultEnabled
	}
	if c.StateFlushInterval <= 0 {
		c.StateFlushInterval = 30 * time.Second
	}
	if c.StateFile == "" {
		baseDir := filepath.Dir(c.ConfigPath)
		c.StateFile = filepath.Join(baseDir, "state", "nodes_state.json")
	}
	if c.SubscriptionRefreshInterval <= 0 && len(c.Subscriptions) > 0 {
		c.SubscriptionRefreshInterval = 24 * time.Hour
	}

	// Load nodes from file if specified
	if c.NodesFile != "" {
		fileNodes, err := loadNodesFromFile(c.NodesFile)
		if err != nil {
			return fmt.Errorf("load nodes from file %q: %w", c.NodesFile, err)
		}
		// Merge file nodes with config nodes
		c.Nodes = append(c.Nodes, fileNodes...)
	}

	// Load nodes from subscriptions
	for _, subURL := range c.Subscriptions {
		subNodes, err := loadNodesFromSubscription(subURL)
		if err != nil {
			log.Printf("⚠️ Failed to load subscription %q: %v (skipping)", subURL, err)
			continue
		}
		log.Printf("✅ Loaded %d nodes from subscription", len(subNodes))
		c.Nodes = append(c.Nodes, subNodes...)
	}

	if len(c.Nodes) == 0 {
		return errors.New("config.nodes cannot be empty (configure nodes in config or use nodes_file)")
	}

	// Deduplicate nodes by endpoint to避免重复
	uniqueNodes, removed := deduplicateNodesByEndpoint(c.Nodes)
	if removed > 0 {
		log.Printf("⚠️ 去重：发现 %d 个主机/IP 重复的节点，已自动跳过", removed)
	}
	if len(uniqueNodes) == 0 {
		return errors.New("all nodes were removed after deduplication; please provide distinct hosts/IPs")
	}
	c.Nodes = uniqueNodes

	portCursor := c.MultiPort.BasePort
	for idx := range c.Nodes {
		c.Nodes[idx].Name = strings.TrimSpace(c.Nodes[idx].Name)
		c.Nodes[idx].URI = strings.TrimSpace(c.Nodes[idx].URI)

		if c.Nodes[idx].URI == "" {
			return fmt.Errorf("node %d is missing uri", idx)
		}

		// Auto-extract name from URI fragment (#name) if not provided
		if c.Nodes[idx].Name == "" {
			if parsed, err := url.Parse(c.Nodes[idx].URI); err == nil && parsed.Fragment != "" {
				// URL decode the fragment to handle encoded characters
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					c.Nodes[idx].Name = decoded
				} else {
					c.Nodes[idx].Name = parsed.Fragment
				}
			}
		}

		// Fallback to default name if still empty
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		if meta, err := parseEndpointMeta(c.Nodes[idx].URI); err == nil {
			c.Nodes[idx].EndpointHost = meta.host
			c.Nodes[idx].EndpointPort = meta.port
			c.Nodes[idx].EndpointScheme = meta.scheme
			c.Nodes[idx].EndpointID = endpointID(meta.host, meta.port)
		} else if c.Nodes[idx].EndpointID == "" && err != nil {
			log.Printf("⚠️ 无法解析节点 %q 的主机信息（URI=%s）：%v", c.Nodes[idx].Name, c.Nodes[idx].URI, err)
		}

		// Auto-assign port in multi-port mode
		if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = portCursor
			portCursor++
		}

		if c.Mode == "multi-port" {
			if c.Nodes[idx].Username == "" {
				c.Nodes[idx].Username = c.MultiPort.Username
				c.Nodes[idx].Password = c.MultiPort.Password
			}
		}
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	return nil
}

const maxDedupLogEntries = 5

// deduplicateNodesByEndpoint removes nodes that share the same host+port.
func deduplicateNodesByEndpoint(nodes []NodeConfig) ([]NodeConfig, int) {
	if len(nodes) == 0 {
		return nodes, 0
	}
	seen := make(map[string]NodeConfig)
	deduped := make([]NodeConfig, 0, len(nodes))
	duplicateLogs := 0
	for _, node := range nodes {
		key := strings.ToLower(node.EndpointID)
		if key == "" {
			deduped = append(deduped, node)
			continue
		}

		if existing, exists := seen[key]; exists {
			if duplicateLogs < maxDedupLogEntries {
				log.Printf("⚠️ 去重：节点 %q (endpoint=%s:%d) 与 %q 重复，已跳过", node.Name, node.EndpointHost, node.EndpointPort, existing.Name)
				duplicateLogs++
			}
			continue
		}

		seen[key] = node
		deduped = append(deduped, node)
	}
	return deduped, len(nodes) - len(deduped)
}

func parseEndpointMeta(rawURI string) (endpointMeta, error) {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return endpointMeta{}, errors.New("empty uri")
	}
	lower := strings.ToLower(rawURI)
	parts := strings.SplitN(lower, "://", 2)
	if len(parts) != 2 {
		return endpointMeta{}, fmt.Errorf("invalid uri: %s", rawURI)
	}
	scheme := parts[0]
	payload := ""
	if idx := strings.Index(rawURI, "://"); idx >= 0 && idx+3 < len(rawURI) {
		payload = rawURI[idx+3:]
	}

	var meta endpointMeta
	meta.scheme = scheme

	switch scheme {
	case "vmess":
		if host, port := parseVMessEndpoint(payload); host != "" {
			meta.host = host
			meta.port = port
			return meta, nil
		}
	case "ss":
		if host, port := parseSSEndpoint(payload); host != "" {
			meta.host = host
			meta.port = port
			return meta, nil
		}
	}

	parsed, err := url.Parse(rawURI)
	if err != nil {
		return endpointMeta{}, err
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return endpointMeta{}, fmt.Errorf("missing host: %s", rawURI)
	}
	port := uint16(0)
	if parsed.Port() != "" {
		if value, err := strconv.Atoi(parsed.Port()); err == nil && value > 0 && value <= 65535 {
			port = uint16(value)
		}
	}
	if port == 0 {
		port = defaultPortForScheme(scheme)
	}
	meta.host = host
	meta.port = port
	return meta, nil
}

func endpointID(host string, port uint16) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func defaultPortForScheme(scheme string) uint16 {
	if port, ok := defaultEndpointPorts[strings.ToLower(scheme)]; ok {
		return port
	}
	return 0
}

func parseVMessEndpoint(payload string) (string, uint16) {
	data, err := decodeMaybeBase64(payload)
	if err != nil {
		return "", 0
	}
	var meta struct {
		Host string `json:"host"`
		Add  string `json:"add"`
		Port any    `json:"port"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", 0
	}
	host := strings.TrimSpace(meta.Host)
	if host == "" {
		host = strings.TrimSpace(meta.Add)
	}
	if host == "" {
		return "", 0
	}
	host = strings.ToLower(host)
	port := parsePort(meta.Port)
	if port == 0 {
		port = defaultPortForScheme("vmess")
	}
	return host, port
}

func parseSSEndpoint(payload string) (string, uint16) {
	base := payload
	if idx := strings.IndexAny(base, "#?"); idx >= 0 {
		base = base[:idx]
	}
	decoded, err := decodeMaybeBase64(base)
	if err != nil {
		return parsePlainSSEndpoint(base)
	}
	return parsePlainSSEndpoint(string(decoded))
}

func parsePlainSSEndpoint(decoded string) (string, uint16) {
	parts := strings.Split(decoded, "@")
	if len(parts) < 2 {
		return "", 0
	}
	hostPort := parts[len(parts)-1]
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(hostPort))
	if err != nil {
		return "", 0
	}
	portVal, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0
	}
	return strings.ToLower(host), uint16(portVal)
}

func decodeMaybeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty base64")
	}
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(s); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func parsePort(value any) uint16 {
	switch v := value.(type) {
	case float64:
		if v <= 0 || v > 65535 {
			return 0
		}
		return uint16(v)
	case int:
		if v <= 0 || v > 65535 {
			return 0
		}
		return uint16(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			return uint16(n)
		}
	}
	return 0
}

// ManagementEnabled reports whether the monitoring endpoint should run.
func (c *Config) ManagementEnabled() bool {
	if c.Management.Enabled == nil {
		return true
	}
	return *c.Management.Enabled
}

// loadNodesFromFile reads a nodes file where each line is a proxy URI
// Lines starting with # are comments, empty lines are ignored
func loadNodesFromFile(path string) ([]NodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseNodesFromContent(string(data))
}

// loadNodesFromSubscription fetches and parses nodes from a subscription URL
// Supports multiple formats: base64 encoded, plain text, clash yaml, etc.
func loadNodesFromSubscription(subURL string) ([]NodeConfig, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set common headers to avoid being blocked
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subscription returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	content := string(body)

	// Try to detect and parse different formats
	return parseSubscriptionContent(content)
}

// parseSubscriptionContent tries to parse subscription content in various formats
func parseSubscriptionContent(content string) ([]NodeConfig, error) {
	content = strings.TrimSpace(content)

	// Check if it's base64 encoded (common for v2ray subscriptions)
	if isBase64(content) {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			// Try URL-safe base64
			decoded, err = base64.RawStdEncoding.DecodeString(content)
			if err != nil {
				// Not base64, try as plain text
				return parseNodesFromContent(content)
			}
		}
		content = string(decoded)
	}

	// Check if it's YAML (Clash format)
	if strings.Contains(content, "proxies:") {
		return parseClashYAML(content)
	}

	// Parse as plain text (one URI per line)
	return parseNodesFromContent(content)
}

// parseNodesFromContent parses nodes from plain text content (one URI per line)
func parseNodesFromContent(content string) ([]NodeConfig, error) {
	var nodes []NodeConfig
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Check if it's a valid proxy URI
		if isProxyURI(line) {
			nodes = append(nodes, NodeConfig{
				URI: line,
			})
		}
	}

	return nodes, nil
}

// isBase64 checks if a string looks like base64 encoded content
func isBase64(s string) bool {
	// Remove whitespace
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return false
	}

	// Base64 should not contain newlines in the middle (unless it's multi-line base64)
	// and should only contain valid base64 characters
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")

	// Check if it contains proxy URI schemes (then it's not base64)
	if strings.Contains(s, "://") {
		return false
	}

	// Try to decode
	_, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		_, err = base64.RawStdEncoding.DecodeString(s)
	}
	return err == nil
}

// isProxyURI checks if a string is a valid proxy URI
func isProxyURI(s string) bool {
	schemes := []string{"vmess://", "vless://", "trojan://", "ss://", "ssr://", "hysteria://", "hysteria2://", "hy2://"}
	for _, scheme := range schemes {
		if strings.HasPrefix(strings.ToLower(s), scheme) {
			return true
		}
	}
	return false
}

// clashConfig represents a minimal Clash configuration for parsing proxies
type clashConfig struct {
	Proxies []clashProxy `yaml:"proxies"`
}

type clashProxy struct {
	Name              string                 `yaml:"name"`
	Type              string                 `yaml:"type"`
	Server            string                 `yaml:"server"`
	Port              int                    `yaml:"port"`
	UUID              string                 `yaml:"uuid"`
	Password          string                 `yaml:"password"`
	Cipher            string                 `yaml:"cipher"`
	AlterId           int                    `yaml:"alterId"`
	Network           string                 `yaml:"network"`
	TLS               bool                   `yaml:"tls"`
	SkipCertVerify    bool                   `yaml:"skip-cert-verify"`
	ServerName        string                 `yaml:"servername"`
	SNI               string                 `yaml:"sni"`
	Flow              string                 `yaml:"flow"`
	UDP               bool                   `yaml:"udp"`
	WSOpts            *clashWSOptions        `yaml:"ws-opts"`
	GrpcOpts          *clashGrpcOptions      `yaml:"grpc-opts"`
	RealityOpts       *clashRealityOptions   `yaml:"reality-opts"`
	ClientFingerprint string                 `yaml:"client-fingerprint"`
	Plugin            string                 `yaml:"plugin"`
	PluginOpts        map[string]interface{} `yaml:"plugin-opts"`
}

type clashWSOptions struct {
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
}

type clashGrpcOptions struct {
	GrpcServiceName string `yaml:"grpc-service-name"`
}

type clashRealityOptions struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

// parseClashYAML parses Clash YAML format and converts to NodeConfig
func parseClashYAML(content string) ([]NodeConfig, error) {
	var clash clashConfig
	if err := yaml.Unmarshal([]byte(content), &clash); err != nil {
		return nil, fmt.Errorf("parse clash yaml: %w", err)
	}

	var nodes []NodeConfig
	for _, proxy := range clash.Proxies {
		uri := convertClashProxyToURI(proxy)
		if uri != "" {
			nodes = append(nodes, NodeConfig{
				Name: proxy.Name,
				URI:  uri,
			})
		}
	}

	return nodes, nil
}

// convertClashProxyToURI converts a Clash proxy config to a standard URI
func convertClashProxyToURI(p clashProxy) string {
	switch strings.ToLower(p.Type) {
	case "vmess":
		return buildVMessURI(p)
	case "vless":
		return buildVLESSURI(p)
	case "trojan":
		return buildTrojanURI(p)
	case "ss", "shadowsocks":
		return buildShadowsocksURI(p)
	case "hysteria2", "hy2":
		return buildHysteria2URI(p)
	default:
		return ""
	}
}

func buildVMessURI(p clashProxy) string {
	params := url.Values{}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.TLS {
		params.Set("security", "tls")
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		} else if p.SNI != "" {
			params.Set("sni", p.SNI)
		}
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("vmess://%s@%s:%d%s#%s", p.UUID, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

func buildVLESSURI(p clashProxy) string {
	params := url.Values{}
	params.Set("encryption", "none")

	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.Flow != "" {
		params.Set("flow", p.Flow)
	}
	if p.TLS {
		params.Set("security", "tls")
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		} else if p.SNI != "" {
			params.Set("sni", p.SNI)
		}
	}
	if p.RealityOpts != nil {
		params.Set("security", "reality")
		if p.RealityOpts.PublicKey != "" {
			params.Set("pbk", p.RealityOpts.PublicKey)
		}
		if p.RealityOpts.ShortID != "" {
			params.Set("sid", p.RealityOpts.ShortID)
		}
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		}
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.GrpcOpts != nil && p.GrpcOpts.GrpcServiceName != "" {
		params.Set("serviceName", p.GrpcOpts.GrpcServiceName)
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", p.UUID, p.Server, p.Port, params.Encode(), url.QueryEscape(p.Name))
}

func buildTrojanURI(p clashProxy) string {
	params := url.Values{}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("allowInsecure", "1")
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("trojan://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

func buildShadowsocksURI(p clashProxy) string {
	// Encode method:password in base64
	userInfo := base64.StdEncoding.EncodeToString([]byte(p.Cipher + ":" + p.Password))
	return fmt.Sprintf("ss://%s@%s:%d#%s", userInfo, p.Server, p.Port, url.QueryEscape(p.Name))
}

func buildHysteria2URI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("insecure", "1")
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("hysteria2://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
}
