package models

import (
	"sort"
	"strings"
)

// 模型特征后缀
var modelFeatureSuffixes = map[string]bool{
	"think":  true,
	"search": true,
}

// 模型变体后缀组合
var modelVariantSuffixes = [][]string{
	{"think"},
	{"search"},
	{"think", "search"},
}

// SplitModelFeatures 分离模型名中的特征后缀
// 返回基础模型名和特征集合
func SplitModelFeatures(model string) (string, map[string]bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", map[string]bool{}
	}
	parts := strings.Split(model, "-")
	features := map[string]bool{}

	for len(parts) > 0 {
		last := strings.ToLower(parts[len(parts)-1])
		if modelFeatureSuffixes[last] {
			features[last] = true
			parts = parts[:len(parts)-1]
		} else {
			break
		}
	}

	if len(features) == 0 {
		return model, features
	}
	return strings.Join(parts, "-"), features
}

// ExpandModelVariants 扩展模型变体列表
func ExpandModelVariants(models []string, excludedModels map[string]bool) []string {
	excluded := make(map[string]bool)
	for k, v := range excludedModels {
		excluded[strings.ToLower(k)] = v
	}

	expanded := []string{}
	for _, model := range models {
		baseModel, features := SplitModelFeatures(model)
		expanded = append(expanded, model)
		if len(features) > 0 || excluded[strings.ToLower(model)] {
			continue
		}
		for _, suffix := range modelVariantSuffixes {
			expanded = append(expanded, baseModel+"-"+strings.Join(suffix, "-"))
		}
	}
	return expanded
}

// ModelRequestsThinking 判断模型是否请求思考模式
func ModelRequestsThinking(model string) bool {
	_, features := SplitModelFeatures(model)
	return features["think"]
}

// ModelRequestsSearch 判断模型是否请求联网搜索
func ModelRequestsSearch(model string) bool {
	_, features := SplitModelFeatures(model)
	return features["search"]
}

// SortedKeys 返回已排序的模型键列表（用于稳定输出）
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
