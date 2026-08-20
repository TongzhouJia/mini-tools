// gcp_egress_checker —— GCP 出站流量 / 免费额度查询器。
//
// 解决的问题：想知道梯子这个月吃掉了多少出站流量，每次都得开浏览器 →
// 结算 → 报告 → 按 SKU 分组 → 在几十行里翻出
// 「Network Standard Data Transfer Out to Internet from Iowa」这一行。
// 一个月看好几次，每次都走一遍。这里把它压成一条命令。
//
// 两个数据源，默认自动挑：
//
//   - Cloud Monitoring（默认，开箱即用）：读实例网卡的 sent_bytes_count。
//     不需要任何额外配置，连历史月份也能查（Monitoring 保留约 6 周）。
//     代价是它是「网卡发出去的字节」，不是账单本身——同一份计量口径，
//     但账单还会扣掉发往 GCP 内部/同区域的部分，所以这个值只会**偏大**，
//     当「还剩多少免费额度」的安全估算刚好。
//
//   - BigQuery 账单导出（可选，和网页上的数字逐字节一致）：直接按 SKU 描述
//     过滤，等价于你在网页上做的那套筛选。需要先在控制台开一次账单导出
//     （只对开启之后的用量生效），配好后设 GCP_EGRESS_BQ_TABLE 即可。
//     没配的话跑 -setup 会打印开启步骤。
//
// 月份边界按**美西时间**算，因为 GCP 的账单月就是这么切的；用 UTC 会在月初
// 月末各差 7~8 小时，正好是最容易让人怀疑人生的地方。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	giB = 1024 * 1024 * 1024

	// GCE 网卡出站字节数。DELTA 型，60 秒一个采样点。
	egressMetric = "compute.googleapis.com/instance/network/sent_bytes_count"

	// 账单上那一行的原文，BigQuery 模式按它精确匹配。
	defaultSKU = "Network Standard Data Transfer Out to Internet from Iowa"

	// GCP 账单月的切分时区。
	defaultTZ = "America/Los_Angeles"

	// Monitoring 只保留 6 周的分钟级原始点，但降采样后的数据能留到 24 个月，
	// 而这里本来就按小时聚合，所以实测半年前的月份照样查得到。
	// 超过保留期查询不会报错，只会返回空数组——那会被显示成「用了 0 字节」，
	// 所以得自己拦一下，把「查不到」和「真没用」区分开。
	monitoringRetention = 24 * 30 * 24 * time.Hour
)

var (
	projectID  string
	monthArg   string
	quotaGiB   float64
	tzName     string
	ratePerGiB float64
	chartDays  int
	source     string
	bqTable    string
	skuDesc    string
	asJSON     bool
	showSetup  bool
)

func init() {
	flag.StringVar(&projectID, "project", envOr("GCP_EGRESS_PROJECT", "gfw-gfy"), "GCP 项目 ID")
	flag.StringVar(&monthArg, "month", "", "计费月 YYYY-MM（默认本月）")
	flag.Float64Var(&quotaGiB, "quota", envFloat("GCP_EGRESS_QUOTA_GIB", 200), "每月免费出站额度（GiB）")
	flag.StringVar(&tzName, "tz", envOr("GCP_EGRESS_TZ", defaultTZ), "账单月所用时区")
	flag.Float64Var(&ratePerGiB, "rate", envFloat("GCP_EGRESS_RATE", 0.085), "超额单价（USD/GiB，Standard Tier 美洲区参考价）")
	flag.IntVar(&chartDays, "days", 14, "按天明细显示最近几天，0 = 不显示")
	flag.StringVar(&source, "source", "auto", "数据源：auto | monitoring | bq")
	flag.StringVar(&bqTable, "bq-table", os.Getenv("GCP_EGRESS_BQ_TABLE"), "账单导出表，形如 project.dataset.gcp_billing_export_v1_XXXXXX")
	flag.StringVar(&skuDesc, "sku", envOr("GCP_EGRESS_SKU", defaultSKU), "BigQuery 模式下要匹配的 SKU 描述")
	flag.BoolVar(&asJSON, "json", false, "输出 JSON，方便脚本消费")
	flag.BoolVar(&showSetup, "setup", false, "打印开启 BigQuery 账单导出的步骤")
}

const usage = `📊 gcp_egress_checker —— 查 GCP 出站流量用了多少、离免费额度还剩多少

用法：
  gcp_egress_checker              查本月
  gcp_egress_checker -month 2026-07   查指定月
  gcp_egress_checker -json        输出 JSON，给脚本消费（月底播报邮件用的就是这个）
  gcp_egress_checker -setup       打印怎么开 BigQuery 账单导出

数据源（-source）：
  auto        先试 monitoring，不行再退 bq（默认）
  monitoring  Cloud Monitoring，快但只有近似值
  bq          BigQuery 账单导出，准但要先开导出（见 -setup）

默认免费额度 200 GiB，超额按 0.085 USD/GiB 估（Standard Tier 美洲区参考价）
账单月按 America/Los_Angeles 算 —— 别用本地时区，会差一天

依赖：gcloud（monitoring 模式）、bq（BigQuery 模式）
环境变量：GCP_EGRESS_PROJECT / _QUOTA_GIB / _TZ / _RATE / _BQ_TABLE / _SKU

参数：
`

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = func() {
		fmt.Print(usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	if showSetup {
		printSetup()
		return
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return fmt.Errorf("时区 %q 无效：%w", tzName, err)
	}

	win, err := resolveWindow(monthArg, loc)
	if err != nil {
		return err
	}

	useBQ, err := pickSource()
	if err != nil {
		return err
	}

	var rep *report
	if useBQ {
		rep, err = fetchFromBigQuery(win)
	} else {
		rep, err = fetchFromMonitoring(win)
	}
	if err != nil {
		return err
	}
	rep.window = win

	if asJSON {
		return emitJSON(rep)
	}
	render(rep)
	return nil
}

// ---------- 时间窗口 ----------

// window 是一个计费月，外加「查到哪儿为止」。查询上界会向下取整到整点：
// 时区偏移都是整小时，这样一小时一个的采样桶才能干净地落进本地自然日，
// 不然按天拆出来的柱子会整体错位几十分钟。
type window struct {
	loc        *time.Location
	month      string    // 2026-08
	start, end time.Time // 计费月的头尾
	until      time.Time // 实际查询上界（本月的话就是「刚才」）
	partial    bool      // 月份还没过完
}

func resolveWindow(arg string, loc *time.Location) (window, error) {
	now := time.Now().In(loc)
	if arg == "" {
		arg = now.Format("2006-01")
	}
	t, err := time.ParseInLocation("2006-01", arg, loc)
	if err != nil {
		return window{}, fmt.Errorf("月份 %q 格式不对，要 YYYY-MM", arg)
	}

	w := window{loc: loc, month: arg, start: t, end: t.AddDate(0, 1, 0)}
	w.until = w.end
	if now.Before(w.end) {
		if now.Before(w.start) {
			return window{}, fmt.Errorf("%s 还没到呢", arg)
		}
		w.until = now.Truncate(time.Hour)
		w.partial = true
	}
	return w, nil
}

// elapsed 返回这个月已经走完的比例，用来把「已用」外推成「月底大概多少」。
func (w window) elapsed() float64 {
	total := w.end.Sub(w.start).Seconds()
	done := w.until.Sub(w.start).Seconds()
	if total <= 0 {
		return 1
	}
	return math.Min(done/total, 1)
}

// ---------- 数据源选择 ----------

func pickSource() (bool, error) {
	switch source {
	case "bq":
		if bqTable == "" {
			return false, fmt.Errorf("-source bq 需要 -bq-table 或 GCP_EGRESS_BQ_TABLE；跑 -setup 看怎么开账单导出")
		}
		if _, err := exec.LookPath("bq"); err != nil {
			return false, fmt.Errorf("找不到 bq 命令（gcloud SDK 自带）")
		}
		return true, nil
	case "monitoring":
		return false, requireGcloud()
	case "auto":
		if bqTable != "" {
			if _, err := exec.LookPath("bq"); err == nil {
				return true, nil
			}
		}
		return false, requireGcloud()
	default:
		return false, fmt.Errorf("-source 只能是 auto / monitoring / bq")
	}
}

func requireGcloud() error {
	if _, err := exec.LookPath("gcloud"); err != nil {
		return fmt.Errorf("找不到 gcloud 命令，先装 Google Cloud SDK")
	}
	return nil
}

// ---------- 汇总结果 ----------

type dayPoint struct {
	Date string  `json:"date"`
	GiB  float64 `json:"gib"`
}

type report struct {
	Source     string             `json:"source"`
	Exact      bool               `json:"exact"` // 是不是账单原值
	TotalGiB   float64            `json:"total_gib"`
	Days       []dayPoint         `json:"days"`
	ByInstance map[string]float64 `json:"by_instance,omitempty"`
	CostUSD    float64            `json:"cost_usd,omitempty"`
	CreditUSD  float64            `json:"credit_usd,omitempty"`
	Note       string             `json:"note,omitempty"`

	window window
}

// ---------- Cloud Monitoring ----------

func fetchFromMonitoring(w window) (*report, error) {
	if time.Since(w.start) > monitoringRetention {
		fmt.Fprintf(os.Stderr, "⚠️  %s 超出 Monitoring 的保留期（约 6 周），数据可能不全或为空\n", w.month)
	}

	token, err := accessToken()
	if err != nil {
		return nil, err
	}

	rep := &report{
		Source:     "Cloud Monitoring（估算，只会偏大）",
		ByInstance: map[string]float64{},
	}
	perDay := map[string]float64{}

	pageToken := ""
	for {
		q := url.Values{}
		q.Set("filter", fmt.Sprintf(`metric.type="%s" AND resource.type="gce_instance"`, egressMetric))
		q.Set("interval.startTime", w.start.UTC().Format(time.RFC3339))
		q.Set("interval.endTime", w.until.UTC().Format(time.RFC3339))
		// 一小时一个桶：对齐区间是从 endTime 往回切的，而 until 已经取整到整点，
		// 所以每个桶都落在整点上，可以直接归到本地自然日里去。
		q.Set("aggregation.alignmentPeriod", "3600s")
		q.Set("aggregation.perSeriesAligner", "ALIGN_SUM")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}

		var resp struct {
			TimeSeries []struct {
				Metric struct {
					Labels map[string]string `json:"labels"`
				} `json:"metric"`
				Points []struct {
					Interval struct {
						StartTime time.Time `json:"startTime"`
					} `json:"interval"`
					Value struct {
						Int64Value string `json:"int64Value"`
					} `json:"value"`
				} `json:"points"`
			} `json:"timeSeries"`
			NextPageToken string `json:"nextPageToken"`
		}
		endpoint := fmt.Sprintf("https://monitoring.googleapis.com/v3/projects/%s/timeSeries?%s",
			url.PathEscape(projectID), q.Encode())
		if err := getJSON(endpoint, token, &resp); err != nil {
			return nil, err
		}

		for _, ts := range resp.TimeSeries {
			name := ts.Metric.Labels["instance_name"]
			if name == "" {
				name = "(未知实例)"
			}
			for _, p := range ts.Points {
				v, err := strconv.ParseFloat(p.Value.Int64Value, 64)
				if err != nil {
					continue
				}
				gib := v / giB
				rep.TotalGiB += gib
				rep.ByInstance[name] += gib
				// 用桶的起点归日：终点是下一个整点，23:00-00:00 那桶会被算进第二天。
				perDay[p.Interval.StartTime.In(w.loc).Format("2006-01-02")] += gib
			}
		}

		pageToken = resp.NextPageToken
		if pageToken == "" {
			break
		}
	}

	if rep.TotalGiB == 0 {
		rep.Note = "没查到任何数据——确认下项目 ID 和实例是不是还在跑"
	}
	rep.Days = sortDays(perDay)
	return rep, nil
}

func accessToken() (string, error) {
	out, err := exec.Command("gcloud", "auth", "print-access-token").Output()
	if err != nil {
		return "", fmt.Errorf("拿不到 access token，先跑 gcloud auth login：%w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func getJSON(endpoint, token string, into any) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("请求 Monitoring API 失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error.Message
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("Monitoring API %d：%s", resp.StatusCode, msg)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// ---------- BigQuery 账单导出 ----------

// 按 invoice.month 过滤，和网页上按账单月分组是同一个字段，所以数字能对上。
// 额外压一段 usage_start_time 范围是为了吃到分区裁剪，别全表扫。
const billingSQL = `
SELECT
  FORMAT_TIMESTAMP('%%Y-%%m-%%d', usage_start_time, @tz) AS day,
  SUM(usage.amount) / POW(1024, 3)                       AS gib,
  SUM(cost)                                              AS cost,
  SUM((SELECT COALESCE(SUM(c.amount), 0) FROM UNNEST(credits) c)) AS credit
FROM ` + "`%s`" + `
WHERE invoice.month = @month
  AND sku.description = @sku
  AND usage_start_time >= @lo
  AND usage_start_time <  @hi
GROUP BY day
ORDER BY day
`

func fetchFromBigQuery(w window) (*report, error) {
	// 账单月的边界是美西时间，但用量记录用 UTC 时间戳，两头各放宽两天保证不切掉。
	lo := w.start.AddDate(0, 0, -2).UTC().Format("2006-01-02 15:04:05")
	hi := w.end.AddDate(0, 0, 2).UTC().Format("2006-01-02 15:04:05")

	// usage.amount 的单位是 bytes；SKU 选定之后不会混进别的单位。
	query := fmt.Sprintf(billingSQL, bqTable)

	args := []string{
		"query", "--nouse_legacy_sql", "--format=json", "--quiet",
		"--parameter=tz:STRING:" + tzName,
		"--parameter=month:STRING:" + strings.ReplaceAll(w.month, "-", ""),
		"--parameter=sku:STRING:" + skuDesc,
		"--parameter=lo:TIMESTAMP:" + lo,
		"--parameter=hi:TIMESTAMP:" + hi,
		query,
	}
	if projectID != "" {
		args = append([]string{"--project_id=" + projectID}, args...)
	}

	cmd := exec.Command("bq", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bq query 失败：%w", err)
	}

	var rows []struct {
		Day    string `json:"day"`
		GiB    string `json:"gib"`
		Cost   string `json:"cost"`
		Credit string `json:"credit"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("解析 bq 输出失败：%w", err)
	}

	rep := &report{
		Source: "BigQuery 账单导出（精确，和网页一致）",
		Exact:  true,
	}
	perDay := map[string]float64{}
	for _, r := range rows {
		g := parseFloat(r.GiB)
		rep.TotalGiB += g
		rep.CostUSD += parseFloat(r.Cost)
		rep.CreditUSD += parseFloat(r.Credit)
		perDay[r.Day] += g
	}
	if len(rows) == 0 {
		rep.Note = fmt.Sprintf("这个月没匹配到 SKU %q——导出可能是在这之后才开的", skuDesc)
	}
	rep.Days = sortDays(perDay)
	return rep, nil
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func sortDays(m map[string]float64) []dayPoint {
	out := make([]dayPoint, 0, len(m))
	for d, g := range m {
		out = append(out, dayPoint{Date: d, GiB: g})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// ---------- 输出 ----------

func render(r *report) {
	w := r.window
	used := r.TotalGiB
	pct := 0.0
	if quotaGiB > 0 {
		pct = used / quotaGiB * 100
	}
	left := quotaGiB - used

	fmt.Printf("\n🌐 GCP 出站流量  项目 %s\n", projectID)
	fmt.Printf("📅 计费月 %s（%s）· 数据源：%s\n", w.month, tzName, r.Source)
	if w.partial {
		fmt.Printf("   统计到 %s\n", w.until.Format("01-02 15:04"))
	}
	fmt.Println()

	mark := "✅"
	switch {
	case pct >= 100:
		mark = "❌"
	case pct >= 80:
		mark = "⚠️"
	}
	fmt.Printf("   已用   %s / %s 免费额度\n", fmtGiB(used), fmtGiB(quotaGiB))
	fmt.Printf("   %s  %s %.1f%%\n", bar(pct, 34), mark, pct)

	if left >= 0 {
		fmt.Printf("   剩余   %s\n", fmtGiB(left))
	} else {
		over := -left
		fmt.Printf("   超额   %s ≈ $%.2f（按 $%.3f/GiB）\n", fmtGiB(over), over*ratePerGiB, ratePerGiB)
	}

	// 预测只在月份没过完时才有意义。
	if w.partial {
		el := w.elapsed()
		if el > 0.02 {
			proj := used / el
			verdict := "✅ 不会超"
			if proj > quotaGiB {
				verdict = fmt.Sprintf("❌ 预计超 %s ≈ $%.2f", fmtGiB(proj-quotaGiB), (proj-quotaGiB)*ratePerGiB)
			} else if proj > quotaGiB*0.85 {
				verdict = "⚠️ 比较悬"
			}
			days := w.end.Sub(w.until).Hours() / 24
			perDay := used / (w.until.Sub(w.start).Hours() / 24)
			fmt.Printf("   日均   %s/天 · 还剩 %.1f 天\n", fmtGiB(perDay), days)
			fmt.Printf("   预测   月底约 %s  %s\n", fmtGiB(proj), verdict)
			if left > 0 && days > 0 {
				fmt.Printf("   余量   接下来每天可用 %s\n", fmtGiB(left/days))
			}
		}
	}

	if r.Exact {
		fmt.Printf("   账单   $%.2f 用量费 · $%.2f 抵扣（含免费额度）→ 实付 $%.2f\n",
			r.CostUSD, -r.CreditUSD, r.CostUSD+r.CreditUSD)
	}

	if len(r.ByInstance) > 1 {
		fmt.Println("\n🖥️  按实例")
		names := make([]string, 0, len(r.ByInstance))
		for n := range r.ByInstance {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return r.ByInstance[names[i]] > r.ByInstance[names[j]] })
		for _, n := range names {
			fmt.Printf("   %-24s %s\n", n, fmtGiB(r.ByInstance[n]))
		}
	}

	if chartDays > 0 && len(r.Days) > 0 {
		days := r.Days
		if len(days) > chartDays {
			days = days[len(days)-chartDays:]
		}
		peak := 0.0
		for _, d := range days {
			peak = math.Max(peak, d.GiB)
		}
		fmt.Printf("\n📊 最近 %d 天\n", len(days))
		for _, d := range days {
			n := 0
			if peak > 0 {
				n = int(math.Round(d.GiB / peak * 24))
			}
			fmt.Printf("   %s  %-24s %s\n", d.Date[5:], strings.Repeat("▊", n), fmtGiB(d.GiB))
		}
	}

	if r.Note != "" {
		fmt.Printf("\n⚠️  %s\n", r.Note)
	}
	if !r.Exact {
		fmt.Printf("\n💡 想要和账单页逐字节一致的数字，跑 %s -setup\n", filepath.Base(os.Args[0]))
	}
	fmt.Println()
}

func emitJSON(r *report) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"project":     projectID,
		"month":       r.window.month,
		"timezone":    tzName,
		"source":      r.Source,
		"exact":       r.Exact,
		"quota_gib":   quotaGiB,
		"used_gib":    r.TotalGiB,
		"left_gib":    quotaGiB - r.TotalGiB,
		"percent":     r.TotalGiB / quotaGiB * 100,
		"partial":     r.window.partial,
		"until":       r.window.until.Format(time.RFC3339),
		"days":        r.Days,
		"by_instance": r.ByInstance,
		"cost_usd":    r.CostUSD,
		"credit_usd":  r.CreditUSD,
		"note":        r.Note,
	})
}

func bar(pct float64, width int) string {
	filled := int(math.Round(pct / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func fmtGiB(g float64) string {
	switch {
	case math.Abs(g) >= 1024:
		return fmt.Sprintf("%.2f TiB", g/1024)
	case math.Abs(g) >= 1:
		return fmt.Sprintf("%.2f GiB", g)
	default:
		return fmt.Sprintf("%.1f MiB", g*1024)
	}
}

func printSetup() {
	fmt.Printf(`
📦 让数字和账单页逐字节一致：开一次 BigQuery 账单导出

1. 建个数据集（放哪个项目都行，这里用 %s）：
     bq --location=US mk -d %s:billing_export

2. 到控制台开导出（这一步没有 CLI，只能点）：
     https://console.cloud.google.com/billing/01607F-B70370-6F6111/export/bigquery
   选「标准使用量费用」→ 项目 %s → 数据集 billing_export → 保存

3. 等几小时出第一批数据，然后拿到表名：
     bq ls %s:billing_export

4. 写进环境变量（表名里的后缀是账单账号 ID 换成下划线）：
     export GCP_EGRESS_BQ_TABLE=%s.billing_export.gcp_billing_export_v1_01607F_B70370_6F6111

配好之后直接跑本工具就会自动切到精确模式。

⚠️  导出只对**开启之后**的用量生效，历史月份补不回来——
    那之前的月份继续用默认的 Monitoring 模式看估算值。

`, projectID, projectID, projectID, projectID, projectID)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
