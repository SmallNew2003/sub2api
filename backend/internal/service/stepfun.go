package service

import (
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
)

// DefaultStepFunTestModel 是连接测试的默认模型；Step Plan 与 payg 均可用。
const DefaultStepFunTestModel = "step-3.5-flash"

// isStepFunCodingBaseURL 只认 StepFun 官方域名下的 Step Plan 基址，避免把
// Step Plan Key 发往自定义中转。coding/v1（Chat）与 coding（Anthropic）
// 两种默认 base 都要识别。
func isStepFunCodingBaseURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "#") {
		return false
	}
	host := strings.ToLower(parsed.Host)
	if host != "api.stepfun.com" && host != "api.stepfun.ai" &&
		host != "api.stepfun.com:443" && host != "api.stepfun.ai:443" {
		return false
	}
	switch parsed.EscapedPath() {
	case "/step_plan", "/step_plan/", "/step_plan/v1", "/step_plan/v1/":
		return true
	default:
		return false
	}
}

// stepfunQuotaURL 从 Step Plan 的推理基址推导订阅用量端点。Chat 基址带
// /v1，Anthropic 基址不带，统一剥掉后拼 /v1/usage。
func stepfunQuotaURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/v1")
	return base + "/v1/usage"
}

// parseStepFunUsageTiers 解析 GET /step_plan/v1/usage。
//
//	quota.credit_buckets[]  → Credit 月池（total/remaining/used_percent/expires_at_ms）
//	quota.windows[]         → 5h / weekly 滚动窗口（used_percent/resets_at_ms）
//	plan.name               → 套餐名（如 Plus）
//
// Credit 套餐通常没有 5h/weekly 窗口；存在 credits 时不再用同名的 monthly
// window 覆盖月池快照。
func parseStepFunUsageTiers(body []byte) ([]CNQuotaTier, string) {
	quota := gjson.GetBytes(body, "quota")
	planName := strings.TrimSpace(gjson.GetBytes(body, "plan.name").String())

	var tiers []CNQuotaTier
	hasMonthly := false

	if buckets := quota.Get("credit_buckets"); buckets.IsArray() {
		var total, remaining float64
		var resetAt string
		count := 0
		buckets.ForEach(func(_, bucket gjson.Result) bool {
			bucketTotal, okTotal := cnParseF64(bucket.Get("total").Value())
			bucketRemaining, okRemaining := cnParseF64(bucket.Get("remaining").Value())
			if !okTotal || !okRemaining || bucketTotal <= 0 {
				return true
			}
			total += bucketTotal
			remaining += bucketRemaining
			if resetAt == "" {
				resetAt = cnNormalizeResetTime(bucket.Get("expires_at_ms").Value())
			}
			count++
			return true
		})
		if count > 0 && total > 0 {
			usedPercent := (total - remaining) / total * 100
			if usedPercent < 0 {
				usedPercent = 0
			}
			tiers = append(tiers, CNQuotaTier{
				Window:      "monthly",
				UsedPercent: usedPercent,
				ResetAt:     resetAt,
			})
			hasMonthly = true
		}
	}

	if windows := quota.Get("windows"); windows.IsArray() {
		windows.ForEach(func(_, window gjson.Result) bool {
			name := stepFunWindowName(window.Get("name").String() + " " + window.Get("type").String())
			if name == "" || (name == "monthly" && hasMonthly) {
				return true
			}
			usedPercent, ok := cnParseF64(window.Get("used_percent").Value())
			if !ok {
				return true
			}
			if usedPercent < 0 {
				usedPercent = 0
			}
			tiers = append(tiers, CNQuotaTier{
				Window:      name,
				UsedPercent: usedPercent,
				ResetAt:     cnNormalizeResetTime(window.Get("resets_at_ms").Value()),
			})
			return true
		})
	}

	return tiers, planName
}

func stepFunWindowName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(name, "5h"), strings.Contains(name, "5_hour"), strings.Contains(name, "five"):
		return "5h"
	case strings.Contains(name, "week"), strings.Contains(name, "7d"):
		return "weekly"
	case strings.Contains(name, "month"), strings.Contains(name, "credit"):
		return "monthly"
	default:
		return ""
	}
}
