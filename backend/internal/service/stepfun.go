package service

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
)

// DefaultStepFunTestModel 是连接测试的默认模型；Step Plan 与 payg 均可用。
const DefaultStepFunTestModel = "step-3.5-flash"

// stepFunQuotaUnavailable 判定 Step Plan 订阅额度是否不可用，语义照搬
// cc-switch 的 usage_script extractor：
//
//	quota.state 必须恰为 "known"；
//	credit_buckets 与 windows 不能同时为空；
//	quota.allowed 为 false 同样视为不可用。
//
// 返回空串表示可用，否则为可直接展示的原因。HTTP 200 只证明 Key 有效，
// 不代表账号开着 Step Plan。
func stepFunQuotaUnavailable(body []byte) string {
	quota := gjson.GetBytes(body, "quota")
	if strings.TrimSpace(quota.Get("state").String()) != "known" {
		return stepFunQuotaUnavailableMessage(body)
	}
	if len(quota.Get("credit_buckets").Array()) == 0 && len(quota.Get("windows").Array()) == 0 {
		return stepFunQuotaUnavailableMessage(body)
	}
	if allowed := quota.Get("allowed"); allowed.Exists() && !allowed.Bool() {
		return stepFunQuotaUnavailableMessage(body)
	}
	return ""
}

// stepFunQuotaUnavailableMessage 复刻 cc-switch 的提示优先级：先取上游
// quota.reason，再按 plan.state 区分「有套餐但无额度数据」与「未开通」。
func stepFunQuotaUnavailableMessage(body []byte) string {
	if reason := strings.TrimSpace(gjson.GetBytes(body, "quota.reason").String()); reason != "" {
		return reason
	}
	if strings.TrimSpace(gjson.GetBytes(body, "plan.state").String()) == "known" {
		return "Step Plan quota has no usage data"
	}
	return "Step Plan is not activated on this account"
}

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
		// used_percent 以上游为准（按 total 加权），缺失时才用 total/remaining
		// 反推——与 cc-switch 取 buckets[0].used_percent 一致。
		var total, remaining, percentWeight, percentSum float64
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
			if usedPercent, ok := cnParseF64(bucket.Get("used_percent").Value()); ok {
				percentWeight += bucketTotal
				percentSum += usedPercent * bucketTotal
			}
			if resetAt == "" {
				resetAt = cnNormalizeResetTime(bucket.Get("expires_at_ms").Value())
			}
			count++
			return true
		})
		if count > 0 && total > 0 {
			usedPercent := (total - remaining) / total * 100
			if percentWeight > 0 {
				usedPercent = percentSum / percentWeight
			}
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
		windows.ForEach(func(key, window gjson.Result) bool {
			name := stepFunWindowLabel(window.Get("name").String(), window.Get("type").String(), int(key.Int()))
			// credit_buckets 已产出 monthly 档时，跳过同名的 monthly 窗口。
			if name == "monthly" && hasMonthly {
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

// stepFunWindowLabel 把上游窗口名归一到 sub2api 的 5h / weekly / monthly
// （调度阈值与 Extra 快照只认这三个）。识别不出时保留原始名的净化形式，
// 对应 cc-switch 的 name || type || "窗口N" 回退，不静默丢窗口；这类标签
// 只参与展示，cnQuotaExtraUpdates 会忽略它们。
func stepFunWindowLabel(name, typ string, index int) string {
	raw := strings.ToLower(strings.TrimSpace(name + " " + typ))
	switch {
	case strings.Contains(raw, "5h"), strings.Contains(raw, "5_hour"), strings.Contains(raw, "five"):
		return "5h"
	case strings.Contains(raw, "week"), strings.Contains(raw, "7d"):
		return "weekly"
	case strings.Contains(raw, "month"), strings.Contains(raw, "credit"):
		return "monthly"
	}
	if label := sanitizeStepFunWindowLabel(name); label != "" {
		return label
	}
	if label := sanitizeStepFunWindowLabel(typ); label != "" {
		return label
	}
	return fmt.Sprintf("window_%d", index+1)
}

func sanitizeStepFunWindowLabel(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_', r == '.':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
