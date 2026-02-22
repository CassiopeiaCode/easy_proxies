package builder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/logx"
	"easy_proxies/internal/store"
	pebblestore "easy_proxies/internal/store/pebble"
	poolout "easy_proxies/internal/outbound/pool"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
)

// Build converts high level config into sing-box Options tree.
func Build(cfg *config.Config) (option.Options, error) {
	baseOutbounds := make([]option.Outbound, 0, len(cfg.Nodes))
	memberTags := make([]string, 0, len(cfg.Nodes))
	metadata := make(map[string]poolout.MemberMeta)
	var failedNodes []string
	usedTags := make(map[string]int) // Track tag usage for uniqueness

	// Best-effort: when store is enabled:
	// - skip nodes already marked as damaged (so they will not be built into sing-box)
	// - mark "build-time" invalid nodes as damaged
	var st store.Store
	if cfg != nil && strings.TrimSpace(cfg.Store.Dir) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var err error
		st, err = pebblestore.Open(ctx, pebblestore.Options{Dir: cfg.Store.Dir})
		cancel()
		if err != nil {
			logx.Printf("⚠️  store open failed, damaged tracking disabled: %v", err)
			st = nil
		}
	}
	if st != nil {
		defer st.Close()
	}

	for _, node := range cfg.Nodes {
		baseTag := sanitizeTag(node.Name)
		if baseTag == "" {
			baseTag = fmt.Sprintf("node-%d", len(memberTags)+1)
		}

		// Ensure tag uniqueness by appending a counter if needed
		tag := baseTag
		if count, exists := usedTags[baseTag]; exists {
			usedTags[baseTag] = count + 1
			tag = fmt.Sprintf("%s-%d", baseTag, count+1)
		} else {
			usedTags[baseTag] = 1
		}

		// If already damaged, do not build into sing-box (damaged changes should accompany reload/import flows).
		if st != nil {
			if host, port, _, hpErr := store.HostPortFromURI(node.URI); hpErr == nil && host != "" && port > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				damaged, dErr := st.IsNodeDamaged(ctx, host, port)
				cancel()
				if dErr != nil {
					logx.Printf("⚠️  query damaged failed for %s:%d: %v (continue building)", host, port, dErr)
				} else if damaged {
					logx.Printf("⚠️  node '%s' is marked damaged in store, skipping build", node.Name)
					failedNodes = append(failedNodes, node.Name)
					continue
				}
			}
		}

		outbound, err := buildNodeOutbound(tag, node.URI, cfg.SkipCertVerify)
		if err != nil {
			logx.Printf("❌ Failed to build node '%s': %v (skipping)", node.Name, err)
			failedNodes = append(failedNodes, node.Name)

			// Build-time invalid: mark damaged (only when we can extract host:port).
			if st != nil {
				if host, port, _, hpErr := store.HostPortFromURI(node.URI); hpErr == nil && host != "" && port > 0 {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					if markErr := st.MarkNodeDamaged(ctx, host, port, err.Error()); markErr != nil {
						logx.Printf("⚠️  mark damaged failed for %s:%d: %v", host, port, markErr)
					}
					cancel()
				}
			}

			continue
		}

		memberTags = append(memberTags, tag)
		baseOutbounds = append(baseOutbounds, outbound)
		meta := poolout.MemberMeta{
			Name: node.Name,
			URI:  node.URI,
			Mode: cfg.Mode,
		}
		// For multi-port and hybrid modes, use per-node port
		if cfg.Mode == "multi-port" || cfg.Mode == "hybrid" {
			meta.ListenAddress = cfg.MultiPort.Address
			meta.Port = node.Port
		} else {
			meta.ListenAddress = cfg.Listener.Address
			meta.Port = cfg.Listener.Port
		}
		metadata[tag] = meta
	}

	// Check if we have at least one valid node
	if len(baseOutbounds) == 0 {
		return option.Options{}, fmt.Errorf("no valid nodes available (all %d nodes failed to build)", len(cfg.Nodes))
	}

	// Log summary
	if len(failedNodes) > 0 {
		logx.Printf("⚠️  %d/%d nodes failed and were skipped: %v", len(failedNodes), len(cfg.Nodes), failedNodes)
	}
	logx.Printf("✅ Successfully built %d/%d nodes", len(baseOutbounds), len(cfg.Nodes))

	// Print proxy links for each node
	printProxyLinks(cfg, metadata)

	var (
		inbounds  []option.Inbound
		outbounds = make([]option.Outbound, len(baseOutbounds))
		route     option.RouteOptions
	)
	copy(outbounds, baseOutbounds)

	// Determine which components to enable based on mode
	enablePoolInbound := cfg.Mode == "pool" || cfg.Mode == "hybrid"
	enableMultiPort := cfg.Mode == "multi-port" || cfg.Mode == "hybrid"
	enableSticky := cfg.Sticky.Enabled

	if !enablePoolInbound && !enableMultiPort && !enableSticky {
		return option.Options{}, fmt.Errorf("unsupported mode %s", cfg.Mode)
	}

	// Build pool inbound (single entry point for all nodes)
	if enablePoolInbound {
		inbound, err := buildPoolInbound(cfg)
		if err != nil {
			return option.Options{}, err
		}
		inbounds = append(inbounds, inbound)
		expected := cfg.Management.ProbeExpectedStatuses
		if len(expected) == 0 && cfg.Management.ProbeExpectedStatus != 0 {
			expected = []int{cfg.Management.ProbeExpectedStatus}
		}
		poolOptions := poolout.Options{
			Mode:              cfg.Pool.Mode,
			Members:           memberTags,
			FailureThreshold:  cfg.Pool.FailureThreshold,
			BlacklistDuration: cfg.Pool.BlacklistDuration,
			Metadata:          metadata,
			ExpectedStatuses:  expected,
		}
		outbounds = append(outbounds, option.Outbound{
			Type:    poolout.Type,
			Tag:     poolout.Tag,
			Options: &poolOptions,
		})
		route.Final = poolout.Tag
	}

	if enableSticky {
		inbound, err := buildStickyInbound(cfg)
		if err != nil {
			return option.Options{}, err
		}
		inbounds = append(inbounds, inbound)
		expected := cfg.Management.ProbeExpectedStatuses
		if len(expected) == 0 && cfg.Management.ProbeExpectedStatus != 0 {
			expected = []int{cfg.Management.ProbeExpectedStatus}
		}
		stickyOptions := poolout.Options{
			Mode:                 "sticky",
			Members:              memberTags,
			FailureThreshold:     cfg.Pool.FailureThreshold,
			BlacklistDuration:    cfg.Pool.BlacklistDuration,
			Metadata:             metadata,
			ExpectedStatuses:     expected,
			StickySwitchInterval: cfg.Sticky.SwitchInterval,
			StickyHistorySize:    cfg.Sticky.HistorySize,
		}
		stickyTag := "proxy-sticky"
		outbounds = append(outbounds, option.Outbound{
			Type:    poolout.Type,
			Tag:     stickyTag,
			Options: &stickyOptions,
		})
		route.Rules = append(route.Rules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				RawDefaultRule: option.RawDefaultRule{
					Inbound: badoption.Listable[string]{"http-sticky-in"},
				},
				RuleAction: option.RuleAction{
					Action: C.RuleActionTypeRoute,
					RouteOptions: option.RouteActionOptions{
						Outbound: stickyTag,
					},
				},
			},
		})
	}

	// Build multi-port inbounds (one port per node)
	if enableMultiPort {
		addr, err := parseAddr(cfg.MultiPort.Address)
		if err != nil {
			return option.Options{}, fmt.Errorf("parse multi-port address: %w", err)
		}
		for _, tag := range memberTags {
			meta := metadata[tag]
			perMeta := map[string]poolout.MemberMeta{tag: meta}
			poolTag := fmt.Sprintf("%s-%s", poolout.Tag, tag)
			expected := cfg.Management.ProbeExpectedStatuses
			if len(expected) == 0 && cfg.Management.ProbeExpectedStatus != 0 {
				expected = []int{cfg.Management.ProbeExpectedStatus}
			}
			perOptions := poolout.Options{
				Mode:              "sequential",
				Members:           []string{tag},
				FailureThreshold:  cfg.Pool.FailureThreshold,
				BlacklistDuration: cfg.Pool.BlacklistDuration,
				Metadata:          perMeta,
				ExpectedStatuses:  expected,
			}
			perPool := option.Outbound{
				Type:    poolout.Type,
				Tag:     poolTag,
				Options: &perOptions,
			}
			outbounds = append(outbounds, perPool)
			inboundOptions := &option.HTTPMixedInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     addr,
					ListenPort: meta.Port,
				},
			}
			username := cfg.MultiPort.Username
			password := cfg.MultiPort.Password
			if username != "" {
				inboundOptions.Users = []auth.User{{Username: username, Password: password}}
			}
			inboundTag := fmt.Sprintf("in-%s", tag)
			inbounds = append(inbounds, option.Inbound{
				Type:    C.TypeHTTP,
				Tag:     inboundTag,
				Options: inboundOptions,
			})
			route.Rules = append(route.Rules, option.Rule{
				Type: C.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{inboundTag},
					},
					RuleAction: option.RuleAction{
						Action: C.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: poolTag,
						},
					},
				},
			})
		}
	}

	opts := option.Options{
		Log:       &option.LogOptions{Level: strings.ToLower(cfg.LogLevel)},
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Route:     &route,
	}
	return opts, nil
}

func buildPoolInbound(cfg *config.Config) (option.Inbound, error) {
	listenAddr, err := parseAddr(cfg.Listener.Address)
	if err != nil {
		return option.Inbound{}, fmt.Errorf("parse listener address: %w", err)
	}
	inboundOptions := &option.HTTPMixedInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     listenAddr,
			ListenPort: cfg.Listener.Port,
		},
	}
	if cfg.Listener.Username != "" {
		inboundOptions.Users = []auth.User{{
			Username: cfg.Listener.Username,
			Password: cfg.Listener.Password,
		}}
	}
	inbound := option.Inbound{
		Type:    C.TypeHTTP,
		Tag:     "http-in",
		Options: inboundOptions,
	}
	return inbound, nil
}

func buildStickyInbound(cfg *config.Config) (option.Inbound, error) {
	listenAddr, err := parseAddr(cfg.Sticky.Address)
	if err != nil {
		return option.Inbound{}, fmt.Errorf("parse sticky listener address: %w", err)
	}
	inboundOptions := &option.HTTPMixedInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     listenAddr,
			ListenPort: cfg.Sticky.Port,
		},
	}
	if cfg.Sticky.Username != "" {
		inboundOptions.Users = []auth.User{{
			Username: cfg.Sticky.Username,
			Password: cfg.Sticky.Password,
		}}
	}
	inbound := option.Inbound{
		Type:    C.TypeHTTP,
		Tag:     "http-sticky-in",
		Options: inboundOptions,
	}
	return inbound, nil
}

func buildNodeOutbound(tag, rawURI string, skipCertVerify bool) (option.Outbound, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.Outbound{}, fmt.Errorf("parse uri: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "vless":
		opts, err := buildVLESSOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeVLESS, Tag: tag, Options: &opts}, nil
	case "hysteria2":
		opts, err := buildHysteria2Options(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeHysteria2, Tag: tag, Options: &opts}, nil
	case "ss", "shadowsocks":
		opts, err := buildShadowsocksOptions(parsed)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeShadowsocks, Tag: tag, Options: &opts}, nil
	case "trojan":
		opts, err := buildTrojanOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeTrojan, Tag: tag, Options: &opts}, nil
	case "vmess":
		opts, err := buildVMessOptions(rawURI, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeVMess, Tag: tag, Options: &opts}, nil
	case "socks", "socks5", "socks4", "socks4a":
		opts, err := buildSOCKSOptions(parsed)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeSOCKS, Tag: tag, Options: &opts}, nil
	case "http", "https":
		opts, err := buildHTTPProxyOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeHTTP, Tag: tag, Options: &opts}, nil
	default:
		return option.Outbound{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
}

func buildHTTPProxyOptions(u *url.URL, skipCertVerify bool) (option.HTTPOutboundOptions, error) {
	if u == nil {
		return option.HTTPOutboundOptions{}, errors.New("nil uri")
	}

	server, port, err := hostPort(u, 8080)
	if err != nil {
		return option.HTTPOutboundOptions{}, err
	}

	opts := option.HTTPOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
	}

	if u.User != nil {
		opts.Username = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			opts.Password = pw
		}
	}

	// If scheme is https, enable outbound TLS.
	if strings.EqualFold(u.Scheme, "https") {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{
			TLS: &option.OutboundTLSOptions{
				Enabled:    true,
				ServerName: server,
				Insecure:   skipCertVerify,
			},
		}
	}

	return opts, nil
}

func buildSOCKSOptions(u *url.URL) (option.SOCKSOutboundOptions, error) {
	if u == nil {
		return option.SOCKSOutboundOptions{}, errors.New("nil uri")
	}

	server, port, err := hostPort(u, 1080)
	if err != nil {
		return option.SOCKSOutboundOptions{}, err
	}

	version := "5"
	switch strings.ToLower(u.Scheme) {
	case "socks4":
		version = "4"
	case "socks4a":
		version = "4a"
	case "socks", "socks5":
		version = "5"
	}

	opts := option.SOCKSOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Version:       version,
		Network:       option.NetworkList(""),
	}

	if u.User != nil {
		opts.Username = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			opts.Password = pw
		}
	}

	return opts, nil
}

// ValidateNodeURI checks whether a node URI can be converted into sing-box outbound options.
// It is intended for "build-time" validation before triggering a reload.
func ValidateNodeURI(rawURI string, skipCertVerify bool) error {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return errors.New("empty uri")
	}
	_, err := buildNodeOutbound("validate", rawURI, skipCertVerify)
	return err
}

func buildVLESSOptions(u *url.URL, skipCertVerify bool) (option.VLESSOutboundOptions, error) {
	uuid := u.User.Username()
	if uuid == "" {
		return option.VLESSOutboundOptions{}, errors.New("vless uri missing uuid in userinfo")
	}
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.VLESSOutboundOptions{}, err
	}
	query := u.Query()
	opts := option.VLESSOutboundOptions{
		UUID:          uuid,
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Network:       option.NetworkList(""),
	}
	if flow := query.Get("flow"); flow != "" {
		opts.Flow = flow
	}
	if packetEncoding := query.Get("packetEncoding"); packetEncoding != "" {
		opts.PacketEncoding = &packetEncoding
	}
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}
	return opts, nil
}

func buildHysteria2Options(u *url.URL, skipCertVerify bool) (option.Hysteria2OutboundOptions, error) {
	password := u.User.String()
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.Hysteria2OutboundOptions{}, err
	}
	query := u.Query()
	opts := option.Hysteria2OutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
	}
	if up := query.Get("upMbps"); up != "" {
		opts.UpMbps = atoiDefault(up)
	}
	if down := query.Get("downMbps"); down != "" {
		opts.DownMbps = atoiDefault(down)
	}
	if obfs := query.Get("obfs"); obfs != "" {
		opts.Obfs = &option.Hysteria2Obfs{Type: obfs, Password: query.Get("obfs-password")}
	}
	opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: hysteriaTLSOptions(server, query, skipCertVerify)}
	return opts, nil
}

func hysteriaTLSOptions(host string, query url.Values, skipCertVerify bool) *option.OutboundTLSOptions {
	tlsOptions := &option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: host,
		Insecure:   skipCertVerify,
	}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("insecure")
	if insecure == "" {
		insecure = query.Get("allowInsecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	return tlsOptions
}

func buildTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	security := strings.ToLower(query.Get("security"))
	if security == "" || security == "none" {
		return nil, nil
	}
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	fp := query.Get("fp")
	if fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}
	if security == "reality" {
		tlsOptions.Reality = &option.OutboundRealityOptions{Enabled: true, PublicKey: query.Get("pbk"), ShortID: query.Get("sid")}
		// Reality requires uTLS; use default fingerprint if not specified
		if tlsOptions.UTLS == nil {
			if fp == "" {
				fp = "chrome"
			}
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
		}
	}
	return tlsOptions, nil
}

func buildV2RayTransport(query url.Values) (*option.V2RayTransportOptions, error) {
	transportType := strings.ToLower(query.Get("type"))
	if transportType == "" || transportType == "tcp" {
		return nil, nil
	}
	options := &option.V2RayTransportOptions{Type: transportType}
	switch transportType {
	case C.V2RayTransportTypeWebsocket:
		wsPath := query.Get("path")
		// 解析 path 中的 early data 参数，如 /path?ed=2048
		if idx := strings.Index(wsPath, "?ed="); idx != -1 {
			edPart := wsPath[idx+4:]
			wsPath = wsPath[:idx]
			// 解析 ed 值
			edValue := edPart
			if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
				edValue = edPart[:ampIdx]
			}
			if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
				options.WebsocketOptions.MaxEarlyData = uint32(ed)
				options.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
			}
		}
		options.WebsocketOptions.Path = wsPath
		if host := query.Get("host"); host != "" {
			options.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	case C.V2RayTransportTypeHTTP:
		options.HTTPOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPOptions.Host = badoption.Listable[string]{host}
		}
	case C.V2RayTransportTypeGRPC:
		options.GRPCOptions.ServiceName = query.Get("serviceName")
	case C.V2RayTransportTypeHTTPUpgrade:
		options.HTTPUpgradeOptions.Path = query.Get("path")
	case "xhttp":
		// XHTTP is not supported by sing-box, fallback to HTTPUpgrade
		logx.Printf("⚠️  XHTTP transport not supported by sing-box, falling back to HTTPUpgrade")
		options.Type = C.V2RayTransportTypeHTTPUpgrade
		options.HTTPUpgradeOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPUpgradeOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	default:
		return nil, fmt.Errorf("unsupported transport type %q", transportType)
	}
	return options, nil
}

func buildShadowsocksOptions(u *url.URL) (option.ShadowsocksOutboundOptions, error) {
	server, port, err := hostPort(u, 8388)
	if err != nil {
		return option.ShadowsocksOutboundOptions{}, err
	}

	// ss:// supports two common userinfo formats:
	// 1) SIP002 base64: ss://<base64(method:password)>@host:port
	// 2) Plain:        ss://method:password@host:port
	//
	// Some subscriptions contain broken/non-SS strings that still look like ss://... and would decode into
	// garbage bytes, which later causes sing-box init errors (e.g. "unknown method: u�").
	userInfo := strings.TrimSpace(u.User.String())
	if userInfo == "" {
		return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks uri missing userinfo")
	}

	var method, password string

	if strings.Contains(userInfo, ":") {
		parts := strings.SplitN(userInfo, ":", 2)
		if len(parts) != 2 {
			return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks userinfo format must be method:password")
		}
		method = parts[0]
		password = parts[1]
	} else {
		decoded, decErr := base64.RawURLEncoding.DecodeString(userInfo)
		if decErr != nil {
			decoded, decErr = base64.StdEncoding.DecodeString(userInfo)
			if decErr != nil {
				return option.ShadowsocksOutboundOptions{}, fmt.Errorf("decode shadowsocks userinfo: %w", decErr)
			}
		}

		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks userinfo format must be method:password")
		}
		method = parts[0]
		password = parts[1]
	}

	method = strings.ToLower(strings.TrimSpace(method))
	password = strings.TrimSpace(password)

	// Method must be ASCII and look like a real cipher name; otherwise fail fast so the node can be skipped/marked damaged.
	if method == "" || password == "" {
		return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks userinfo missing method or password")
	}
	for _, r := range method {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return option.ShadowsocksOutboundOptions{}, fmt.Errorf("invalid shadowsocks method %q", method)
	}

	opts := option.ShadowsocksOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Method:        method,
		Password:      password,
	}

	query := u.Query()
	if plugin := query.Get("plugin"); plugin != "" {
		opts.Plugin = plugin
		opts.PluginOptions = query.Get("plugin-opts")
	}

	return opts, nil
}

func buildTrojanOptions(u *url.URL, skipCertVerify bool) (option.TrojanOutboundOptions, error) {
	password := u.User.Username()
	if password == "" {
		return option.TrojanOutboundOptions{}, errors.New("trojan uri missing password in userinfo")
	}

	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.TrojanOutboundOptions{}, err
	}

	query := u.Query()
	opts := option.TrojanOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
		Network:       option.NetworkList(""),
	}

	// Parse TLS options
	if tlsOptions, err := buildTrojanTLSOptions(query, skipCertVerify); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	// Parse transport options
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	return opts, nil
}

// vmessJSON represents the JSON structure of a VMess URI
type vmessJSON struct {
	V    interface{} `json:"v"`    // Version, can be string or int
	PS   string      `json:"ps"`   // Remarks/name
	Add  string      `json:"add"`  // Server address
	Port interface{} `json:"port"` // Server port, can be string or int
	ID   string      `json:"id"`   // UUID
	Aid  interface{} `json:"aid"`  // Alter ID, can be string or int
	Scy  string      `json:"scy"`  // Security/cipher
	Net  string      `json:"net"`  // Network type (tcp, ws, etc.)
	Type string      `json:"type"` // Header type
	Host string      `json:"host"` // Host header
	Path string      `json:"path"` // Path
	TLS  string      `json:"tls"`  // TLS (tls or empty)
	SNI  string      `json:"sni"`  // SNI
	ALPN string      `json:"alpn"` // ALPN
	FP   string      `json:"fp"`   // Fingerprint
}

func (v *vmessJSON) GetPort() int {
	switch p := v.Port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case string:
		port, _ := strconv.Atoi(p)
		return port
	}
	return 443
}

func (v *vmessJSON) GetAlterId() int {
	switch a := v.Aid.(type) {
	case float64:
		return int(a)
	case int:
		return a
	case string:
		aid, _ := strconv.Atoi(a)
		return aid
	}
	return 0
}

func buildVMessOptions(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	// Remove vmess:// prefix
	encoded := strings.TrimPrefix(rawURI, "vmess://")

	// Try to decode as base64 JSON (standard format)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			// Try as URL format: vmess://uuid@server:port?...
			return buildVMessOptionsFromURL(rawURI, skipCertVerify)
		}
	}

	var vmess vmessJSON
	if err := json.Unmarshal(decoded, &vmess); err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess json: %w", err)
	}

	if vmess.Add == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing server address")
	}
	if vmess.ID == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing uuid")
	}

	port := vmess.GetPort()
	if port == 0 {
		port = 443
	}

	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     vmess.Add,
			ServerPort: uint16(port),
		},
		UUID:     vmess.ID,
		AlterId:  vmess.GetAlterId(),
		Security: vmess.Scy,
	}

	// Default security
	if opts.Security == "" {
		opts.Security = "auto"
	}

	// Build transport options
	// VMess JSON "net" must be a valid sing-box v2ray transport.
	// Unknown values (e.g. "none") should fail fast so the node can be marked damaged and skipped.
	if vmess.Net != "" && vmess.Net != "tcp" {
		transport := &option.V2RayTransportOptions{}
		switch vmess.Net {
		case "ws":
			transport.Type = C.V2RayTransportTypeWebsocket
			wsPath := vmess.Path
			// Handle early data in path
			if idx := strings.Index(wsPath, "?ed="); idx != -1 {
				edPart := wsPath[idx+4:]
				wsPath = wsPath[:idx]
				edValue := edPart
				if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
					edValue = edPart[:ampIdx]
				}
				if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
					transport.WebsocketOptions.MaxEarlyData = uint32(ed)
					transport.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
				}
			}
			transport.WebsocketOptions.Path = wsPath
			if vmess.Host != "" {
				transport.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {vmess.Host}}
			}
		case "h2":
			transport.Type = C.V2RayTransportTypeHTTP
			transport.HTTPOptions.Path = vmess.Path
			if vmess.Host != "" {
				transport.HTTPOptions.Host = badoption.Listable[string]{vmess.Host}
			}
		case "grpc":
			transport.Type = C.V2RayTransportTypeGRPC
			transport.GRPCOptions.ServiceName = vmess.Path
		default:
			return option.VMessOutboundOptions{}, fmt.Errorf("unsupported transport type %q", vmess.Net)
		}
		opts.Transport = transport
	}

	// Build TLS options
	if vmess.TLS == "tls" {
		tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
		if vmess.SNI != "" {
			tlsOptions.ServerName = vmess.SNI
		} else if vmess.Host != "" {
			tlsOptions.ServerName = vmess.Host
		}
		if vmess.ALPN != "" {
			tlsOptions.ALPN = badoption.Listable[string](strings.Split(vmess.ALPN, ","))
		}
		if vmess.FP != "" {
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: vmess.FP}
		}
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildVMessOptionsFromURL(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess url: %w", err)
	}

	uuid := parsed.User.Username()
	if uuid == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess uri missing uuid")
	}

	server, port, err := hostPort(parsed, 443)
	if err != nil {
		return option.VMessOutboundOptions{}, err
	}

	query := parsed.Query()
	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     server,
			ServerPort: uint16(port),
		},
		UUID:     uuid,
		Security: query.Get("encryption"),
	}

	if opts.Security == "" {
		opts.Security = "auto"
	}

	if aid := query.Get("alterId"); aid != "" {
		opts.AlterId, _ = strconv.Atoi(aid)
	}

	// Build transport
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	// Build TLS
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildTrojanTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	// Trojan always uses TLS by default
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}

	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	if peer := query.Get("peer"); peer != "" && tlsOptions.ServerName == "" {
		tlsOptions.ServerName = peer
	}

	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}

	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}

	if fp := query.Get("fp"); fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}

	return tlsOptions, nil
}

func hostPort(u *url.URL, defaultPort int) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("missing host")
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = strconv.Itoa(defaultPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

func parseAddr(value string) (*badoption.Addr, error) {
	addr := strings.TrimSpace(value)
	if addr == "" {
		return nil, nil
	}
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	bad := badoption.Addr(parsed)
	return &bad, nil
}

func sanitizeTag(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimSpace(lower)
	if lower == "" {
		return ""
	}
	segments := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	result := strings.Join(segments, "-")
	result = strings.Trim(result, "-")
	return result
}

func atoiDefault(value string) int {
	if strings.HasSuffix(value, "mbps") {
		value = strings.TrimSuffix(value, "mbps")
	}
	if strings.HasSuffix(value, "Mbps") {
		value = strings.TrimSuffix(value, "Mbps")
	}
	v, _ := strconv.Atoi(value)
	return v
}

// printProxyLinks prints all proxy connection information at startup
func printProxyLinks(cfg *config.Config, metadata map[string]poolout.MemberMeta) {
	const maxListLines = 50

	logx.Println("")
	logx.Println("📡 Proxy Links:")
	logx.Println("═══════════════════════════════════════════════════════════════")

	showPoolEntry := cfg.Mode == "pool" || cfg.Mode == "hybrid"
	showMultiPort := cfg.Mode == "multi-port" || cfg.Mode == "hybrid"

	if showPoolEntry {
		// Pool mode: single entry point for all nodes
		var auth string
		if cfg.Listener.Username != "" {
			auth = fmt.Sprintf("%s:%s@", cfg.Listener.Username, cfg.Listener.Password)
		}
		proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.Listener.Address, cfg.Listener.Port)
		logx.Printf("🌐 Pool Entry Point:")
		logx.Printf("   %s", proxyURL)
		logx.Println("")

		total := len(metadata)
		logx.Printf("   Nodes in pool (%d):", total)

		// metadata is a map; order is not stable. We only print a small sample to avoid huge logs and slow startups.
		shown := 0
		for _, meta := range metadata {
			logx.Printf("   • %s", meta.Name)
			shown++
			if shown >= maxListLines {
				break
			}
		}
		if total > shown {
			logx.Printf("   ... (%d more omitted)", total-shown)
		}

		if showMultiPort {
			logx.Println("")
		}
	}

	if cfg.Sticky.Enabled {
		var auth string
		if cfg.Sticky.Username != "" {
			auth = fmt.Sprintf("%s:%s@", cfg.Sticky.Username, cfg.Sticky.Password)
		}
		stickyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.Sticky.Address, cfg.Sticky.Port)
		logx.Printf("🧷 Sticky Entry Point:")
		logx.Printf("   %s", stickyURL)
		logx.Printf("   rotate interval: %s, history size: %d", cfg.Sticky.SwitchInterval, cfg.Sticky.HistorySize)
		logx.Println("")
	}

	if showMultiPort {
		// Multi-port mode: each node has its own port
		total := len(cfg.Nodes)
		logx.Printf("🔌 Multi-Port Entry Points (%d nodes):", total)
		logx.Println("")

		shown := 0
		for _, node := range cfg.Nodes {
			var auth string
			username := node.Username
			password := node.Password
			if username == "" {
				username = cfg.MultiPort.Username
				password = cfg.MultiPort.Password
			}
			if username != "" {
				auth = fmt.Sprintf("%s:%s@", username, password)
			}
			proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.MultiPort.Address, node.Port)
			logx.Printf("   [%d] %s", node.Port, node.Name)
			logx.Printf("       %s", proxyURL)

			shown++
			if shown >= maxListLines {
				break
			}
		}
		if total > shown {
			logx.Printf("   ... (%d more omitted)", total-shown)
		}
	}

	logx.Println("═══════════════════════════════════════════════════════════════")
	logx.Println("")
}
