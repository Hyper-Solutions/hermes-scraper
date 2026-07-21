package hermes

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Small helpers for coercing values out of decoded bck.hermes.com JSON, used by
// the listing and menu parsers. Product detail is returned raw (unnormalized),
// so no product/variant mapping lives here.

func piDecodeObject(raw json.RawMessage) map[string]any {
	var out map[string]any
	if err := piDecodeJSON(raw, &out); err != nil {
		return nil
	}
	return out
}

func piDecodeJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func piAsMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

func piAsArray(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

func piAsString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func piAsStringSlice(value any) []string {
	var out []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if s := piAsString(item); s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, typed...)
	case string:
		if typed != "" {
			out = append(out, typed)
		}
	}
	return out
}

func piFloatPtr(value any) *float64 {
	switch typed := value.(type) {
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return &f
		}
	case float64:
		return &typed
	case int:
		f := float64(typed)
		return &f
	case string:
		if typed == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(typed, 64); err == nil {
			return &f
		}
	}
	return nil
}

func piFirstAssetURL(value any) string {
	for _, item := range piAsArray(value) {
		asset := piAsMap(item)
		if len(asset) == 0 {
			continue
		}
		if piAsString(asset["type"]) == "image" || piAsString(asset["url"]) != "" {
			return piNormalizeAssetURL(piAsString(asset["url"]))
		}
	}
	return ""
}

func piNormalizeHermesURL(value, baseHref string) string {
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	if strings.HasPrefix(value, "//") {
		return "https:" + value
	}
	if strings.HasPrefix(value, "/product/") && baseHref != "" {
		return "https://www.hermes.com" + strings.TrimRight(baseHref, "/") + value
	}
	if strings.HasPrefix(value, "/") {
		return "https://www.hermes.com" + value
	}
	return value
}

func piNormalizeAssetURL(value string) string {
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "//") {
		return "https:" + value
	}
	return value
}

func piFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
