package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

// 借鉴自 opencode2api models.go：从 models.opencode.ai/api.json 同步每个模型
// 的原生协议（chat/responses/anthropic）。cline-proxy 的 zen 渠道固定走 Chat，
// 因此只保留 protocol=chat 的模型，过滤掉原生协议为 responses/anthropic 的模型。

const (
	openCodeCapabilitiesURL = "https://models.opencode.ai/api.json"
	openCodeZenDocsURL      = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"
	openCodeGoDocsURL       = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/go.mdx"
	capabilitiesTimeout     = 30 * time.Second
)

// protocolDocEndpointPattern 匹配 mdx 文档表格行：|模型名|`endpoint路径`|
// 捕获组1=模型ID，组2=端点尾部(chat/completions|responses|messages)。
// 用双引号字符串避免反引号拼接混乱（反引号在双引号串内是普通字符）。
var protocolDocEndpointPattern = regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages)`")

type protocolCapabilities struct {
	Protocols   map[Tier]map[string]Protocol
	Unsupported map[Tier]map[string]bool
}

type capabilityProvider struct {
	ID     string                      `json:"id"`
	API    string                      `json:"api"`
	NPM    string                      `json:"npm"`
	Models map[string]capabilityModel  `json:"models"`
}

type capabilityModel struct {
	ID       string                    `json:"id"`
	Provider *capabilityModelProvider  `json:"provider"`
}

type capabilityModelProvider struct {
	NPM string `json:"npm"`
}

// fetchProtocolCapabilities 拉取 OpenCode 能力目录，返回每个模型的原生协议。
// 机器目录为主源，GitHub mdx 文档为补充源。
func fetchProtocolCapabilities(ctx context.Context, endpoint string) (protocolCapabilities, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return protocolCapabilities{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/1.18.21 (%s %s; %s)", runtime.GOOS, runtime.GOARCH, runtime.Version()))
	resp, err := kit.HTTPClient.Do(req)
	if err != nil {
		return protocolCapabilities{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return protocolCapabilities{}, fmt.Errorf("OpenCode capability endpoint returned HTTP %d", resp.StatusCode)
	}
	var providers map[string]capabilityProvider
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err := dec.Decode(&providers); err != nil {
		return protocolCapabilities{}, err
	}
	result := protocolCapabilities{
		Protocols:   map[Tier]map[string]Protocol{TierZen: {}, TierGo: {}},
		Unsupported: map[Tier]map[string]bool{TierZen: {}, TierGo: {}},
	}
	for providerID, provider := range providers {
		tier, ok := capabilityTier(providerID, provider.API)
		if !ok {
			continue
		}
		for modelID, model := range provider.Models {
			if model.ID != "" {
				modelID = model.ID
			}
			npm := provider.NPM
			if model.Provider != nil && model.Provider.NPM != "" {
				npm = model.Provider.NPM
			}
			if protocol, ok := protocolForSDK(npm); ok {
				result.Protocols[tier][modelID] = protocol
			} else {
				result.Unsupported[tier][modelID] = true
			}
		}
	}
	// mdx 文档作为补充源，覆盖机器目录的默认 SDK 判定
	for _, doc := range []struct {
		tier Tier
		url  string
	}{
		{TierZen, openCodeZenDocsURL},
		{TierGo, openCodeGoDocsURL},
	} {
		protocols, err := fetchProtocolDocs(ctx, doc.url)
		if err != nil {
			continue
		}
		for modelID, protocol := range protocols {
			result.Protocols[doc.tier][modelID] = protocol
			delete(result.Unsupported[doc.tier], modelID)
		}
	}
	if len(result.Protocols[TierZen]) == 0 && len(result.Protocols[TierGo]) == 0 && len(result.Unsupported[TierZen]) == 0 && len(result.Unsupported[TierGo]) == 0 {
		return protocolCapabilities{}, errors.New("OpenCode capability endpoint returned no Zen or Go models")
	}
	return result, nil
}

func fetchProtocolDocs(ctx context.Context, endpoint string) (map[string]Protocol, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, text/markdown, */*")
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/1.18.21 (%s %s; %s)", runtime.GOOS, runtime.GOARCH, runtime.Version()))
	resp, err := kit.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OpenCode endpoint documentation returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	result := make(map[string]Protocol)
	for _, line := range strings.Split(string(body), "\n") {
		match := protocolDocEndpointPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		modelID := strings.TrimSpace(match[1])
		if modelID == "" || strings.ContainsAny(modelID, " `|") {
			continue
		}
		var protocol Protocol
		switch match[2] {
		case "chat/completions":
			protocol = ProtocolChat
		case "responses":
			protocol = ProtocolResponses
		case "messages":
			protocol = ProtocolAnthropic
		}
		if protocol != "" {
			result[modelID] = protocol
		}
	}
	if len(result) == 0 {
		return nil, errors.New("OpenCode endpoint documentation returned no protocol rows")
	}
	return result, nil
}

func capabilityTier(providerID, api string) (Tier, bool) {
	value := strings.ToLower(strings.TrimSpace(providerID + " " + api))
	if strings.Contains(value, "opencode-go") || strings.Contains(value, "/go/") {
		return TierGo, true
	}
	if strings.Contains(value, "opencode") || strings.Contains(value, "/zen/") {
		return TierZen, true
	}
	return "", false
}

// protocolForSDK 按 npm 包名映射原生协议。
// 借鉴自 opencode2api models.go:564。
func protocolForSDK(npm string) (Protocol, bool) {
	value := strings.ToLower(strings.TrimSpace(npm))
	switch {
	case strings.Contains(value, "anthropic"):
		return ProtocolAnthropic, true
	case value == "@ai-sdk/openai" || strings.HasSuffix(value, "/openai"):
		return ProtocolResponses, true
	case strings.Contains(value, "openai-compatible"):
		return ProtocolChat, true
	default:
		return "", false
	}
}

// zenProtocolFor 查询模型的原生协议。能力目录未命中则默认 chat。
func zenProtocolFor(modelID string) Protocol {
	if p, ok := zenProtocolTable[modelID]; ok {
		return p
	}
	return ProtocolChat
}

// zenProtocolTable 内存协议表（由 syncZenProtocols 填充）。
var zenProtocolTable = map[string]Protocol{}

// syncZenProtocols 拉取能力目录，填充 zen 协议表。
// 失败仅日志，不阻断（fallback 全 chat）。
func syncZenProtocols() {
	ctx, cancel := context.WithTimeout(context.Background(), capabilitiesTimeout)
	defer cancel()
	caps, err := fetchProtocolCapabilities(ctx, openCodeCapabilitiesURL)
	if err != nil {
		log.Printf("zen protocol sync: failed (%v), fallback to all-chat", err)
		return
	}
	table := map[string]Protocol{}
	for modelID, protocol := range caps.Protocols[TierZen] {
		table[modelID] = protocol
	}
	zenProtocolTable = table
	log.Printf("zen protocol sync: %d models (chat=%d, anthropic=%d, responses=%d)",
		len(table),
		countProtocol(table, ProtocolChat),
		countProtocol(table, ProtocolAnthropic),
		countProtocol(table, ProtocolResponses))
}

func countProtocol(table map[string]Protocol, p Protocol) int {
	n := 0
	for _, v := range table {
		if v == p {
			n++
		}
	}
	return n
}
