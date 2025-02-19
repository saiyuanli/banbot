package opt

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"github.com/banbox/banbot/orm"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/banbox/banbot/biz"
	"github.com/banbox/banbot/btime"
	"github.com/banbox/banbot/config"
	"github.com/banbox/banbot/core"
	"github.com/banbox/banbot/orm/ormo"
	"github.com/banbox/banbot/strat"
	"github.com/banbox/banbot/utils"
	"github.com/banbox/banexg/errs"
	"github.com/banbox/banexg/log"
	utils2 "github.com/banbox/banexg/utils"
	"github.com/olekukonko/tablewriter"
	"go.uber.org/zap"
)

type BTResult struct {
	MaxOpenOrders   int        `json:"maxOpenOrders"`
	MinReal         float64    `json:"minReal"`
	MaxReal         float64    `json:"maxReal"`         // Maximum Assets 最大资产
	MaxDrawDownPct  float64    `json:"maxDrawDownPct"`  // Maximum drawdown percentage 最大回撤百分比
	ShowDrawDownPct float64    `json:"showDrawDownPct"` // Displays the maximum drawdown percentage 显示最大回撤百分比
	MaxDrawDownVal  float64    `json:"maxDrawDownVal"`  // Maximum drawdown percentage 最大回撤金额
	ShowDrawDownVal float64    `json:"showDrawDownVal"` // Displays the maximum drawdown percentage 显示最大回撤金额
	MaxFundOccup    float64    `json:"maxFundOccup"`
	MaxOccupForPair float64    `json:"maxOccupForPair"`
	BarNum          int        `json:"barNum"`
	TimeNum         int        `json:"timeNum"`
	OrderNum        int        `json:"orderNum"`
	lastTime        int64      // 上次bar的时间戳
	histOdOff       int        // 计算已完成订单利润的偏移
	donePftLegal    float64    // 已完成订单利润
	Plots           *PlotData  `json:"plots"`
	CreateMS        int64      `json:"createMS"`
	StartMS         int64      `json:"startMS"`
	EndMS           int64      `json:"endMS"`
	PlotEvery       int        `json:"plotEvery"`
	TotalInvest     float64    `json:"totalInvest"`
	OutDir          string     `json:"outDir"`
	PairGrps        []*RowItem `json:"pairGrps"`
	DateGrps        []*RowItem `json:"dateGrps"`
	EnterGrps       []*RowItem `json:"enterGrps"`
	ExitGrps        []*RowItem `json:"exitGrps"`
	ProfitGrps      []*RowItem `json:"profitGrps"`
	TotProfit       float64    `json:"totProfit"`
	TotCost         float64    `json:"totCost"`
	TotFee          float64    `json:"totFee"`
	TotProfitPct    float64    `json:"totProfitPct"`
	WinRatePct      float64    `json:"winRatePct"`
	SharpeRatio     float64    `json:"sharpeRatio"`
	SortinoRatio    float64    `json:"sortinoRatio"`
}

type PlotData struct {
	Labels        []string  `json:"labels"`
	OdNum         []int     `json:"odNum"`
	JobNum        []int     `json:"jobNum"`
	Real          []float64 `json:"real"`
	Available     []float64 `json:"available"`
	Profit        []float64 `json:"profit"`
	UnrealizedPOL []float64 `json:"unrealizedPOL"`
	WithDraw      []float64 `json:"withDraw"`
	tmpOdNum      int
}

type RowPart struct {
	WinCount     int                `json:"winCount"`
	OrderNum     int                `json:"orderNum"`
	ProfitSum    float64            `json:"profitSum"`
	ProfitPctSum float64            `json:"profitPctSum"`
	CostSum      float64            `json:"costSum"`
	Durations    []int              `json:"durations"`
	Orders       []*ormo.InOutOrder `json:"-"`
	Sharpe       float64            `json:"sharpe"` // 夏普比率
	Sortino      float64            `json:"sortino"`
}

type RowItem struct {
	Title string `json:"title"`
	RowPart
}

var (
	PairPickers = make(map[string]func(r *BTResult) []string)
)

func NewBTResult() *BTResult {
	res := &BTResult{
		Plots:     &PlotData{},
		PlotEvery: 1,
		CreateMS:  btime.UTCStamp(),
	}
	return res
}

func (r *BTResult) printBtResult() {
	if config.StratPerf != nil && config.StratPerf.Enable {
		core.DumpPerfs(r.OutDir)
	}

	r.Collect()
	orders := ormo.HistODs
	var b strings.Builder
	var tblText string
	if len(orders) > 0 {
		items := []struct {
			Title  string
			Handle func(*BTResult) string
		}{
			{Title: " Pair Profits ", Handle: textGroupPairs},
			{Title: " Date Profits ", Handle: textGroupDays},
			{Title: " Profit Ranges ", Handle: textGroupProfitRanges},
			{Title: " Enter Tag ", Handle: textGroupEntTags},
			{Title: " Exit Tag ", Handle: textGroupExitTags},
		}
		for _, item := range items {
			tblText = item.Handle(r)
			if tblText != "" {
				width := strings.Index(tblText, "\n")
				head := utils.PadCenter(item.Title, width, "=")
				b.WriteString(head)
				b.WriteString("\n")
				b.WriteString(tblText)
				b.WriteString("\n")
			}
		}
	} else {
		b.WriteString("No Orders Found\n")
	}
	b.WriteString(r.textMetrics(orders))
	log.Info("BackTest Reports:\n" + b.String())

	r.dumpBtFiles()
}

func (r *BTResult) dumpBtFiles() {
	csvPath := fmt.Sprintf("%s/orders.csv", r.OutDir)
	err_ := DumpOrdersCSV(ormo.HistODs, csvPath)
	if err_ != nil {
		log.Error("dump orders.csv fail", zap.Error(err_))
	}

	err := ormo.DumpOrdersGob(filepath.Join(r.OutDir, "orders.gob"))
	if err != nil {
		log.Warn("dump orders.gob fail", zap.Error(err))
	}

	r.dumpConfig()

	r.dumpStrategy()

	r.dumpStratOutputs()

	r.dumpGraph()

	r.dumpDetail("")
}

func (r *BTResult) Collect() {
	orders := ormo.HistODs
	r.OrderNum = len(orders)
	sumProfit := float64(0)
	sumFee := float64(0)
	sumCost := float64(0)
	winCount := float64(0)
	for _, od := range orders {
		sumProfit += od.Profit
		sumFee += od.Enter.Fee
		if od.Exit != nil {
			sumFee += od.Exit.Fee
		}
		sumCost += od.EnterCost() / od.Leverage
		if od.Profit > 0 {
			winCount += 1
		}
	}
	r.TotProfit = sumProfit
	r.TotCost = utils.NanInfTo(sumCost, 0)
	r.TotFee = sumFee
	r.TotProfitPct = r.TotProfit * 100 / r.TotalInvest
	if r.MinReal > r.MaxReal {
		r.MinReal = r.MaxReal
	}
	// Calculate the maximum drawdown on the chart
	// 计算图表上的最大回撤
	ddRate, ddVal := r.Plots.calcDrawDown()
	r.ShowDrawDownPct = ddRate * 100
	r.ShowDrawDownVal = ddVal
	if len(orders) > 0 {
		r.WinRatePct = winCount * 100 / float64(len(orders))
		r.groupByPairs(orders)
		r.groupByDates(orders)
		r.groupByProfits(orders)
		r.groupByEnters(orders)
		r.groupByExits(orders)
	}
	err := r.calcMeasures(30)
	if err != nil {
		log.Warn("calc sharpe/sortino fail", zap.Error(err))
	}
}

func (r *BTResult) textMetrics(orders []*ormo.InOutOrder) string {
	var b bytes.Buffer
	table := tablewriter.NewWriter(&b)
	heads := []string{"Metric", "Value"}
	table.SetHeader(heads)
	table.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
	table.SetCenterSeparator("|")
	table.SetAlignment(tablewriter.ALIGN_RIGHT)
	table.Append([]string{"Backtest From", btime.ToDateStr(r.StartMS, "")})
	table.Append([]string{"Backtest To", btime.ToDateStr(r.EndMS, "")})
	table.Append([]string{"Max Open Orders", strconv.Itoa(r.MaxOpenOrders)})
	table.Append([]string{"Total Orders/BarNum", fmt.Sprintf("%v/%v", len(orders), r.BarNum)})
	table.Append([]string{"Total Investment", strconv.FormatFloat(r.TotalInvest, 'f', 0, 64)})
	wallets := biz.GetWallets(config.DefAcc)
	finBalance := wallets.AvaLegal(nil)
	table.Append([]string{"Final Balance", strconv.FormatFloat(finBalance, 'f', 2, 64)})
	finWithDraw := wallets.GetWithdrawLegal(nil)
	table.Append([]string{"Final WithDraw", strconv.FormatFloat(finWithDraw, 'f', 2, 64)})
	table.Append([]string{"Absolute Profit", strconv.FormatFloat(r.TotProfit, 'f', 2, 64)})
	totProfitPct := strconv.FormatFloat(r.TotProfitPct, 'f', 1, 64)
	table.Append([]string{"Total Profit %", totProfitPct + "%"})
	table.Append([]string{"Total Fee", strconv.FormatFloat(r.TotFee, 'f', 2, 64)})
	avfProfit := strconv.FormatFloat(r.TotProfitPct*100/float64(len(orders)), 'f', 2, 64)
	table.Append([]string{"Avg Profit %%", avfProfit + "%%"})
	table.Append([]string{"Total Cost", strconv.FormatFloat(r.TotCost, 'f', 2, 64)})
	avgCost := r.TotCost / float64(len(orders))
	table.Append([]string{"Avg Cost", strconv.FormatFloat(avgCost, 'f', 2, 64)})
	slices.SortFunc(orders, func(a, b *ormo.InOutOrder) int {
		return int((a.Profit - b.Profit) * 100)
	})
	if len(orders) > 0 {
		worstVal := strconv.FormatFloat(orders[0].Profit, 'f', 1, 64)
		worstPct := strconv.FormatFloat(orders[0].ProfitRate*100, 'f', 1, 64)
		bestVal := strconv.FormatFloat(orders[len(orders)-1].Profit, 'f', 1, 64)
		bestPct := strconv.FormatFloat(orders[len(orders)-1].ProfitRate*100, 'f', 1, 64)
		table.Append([]string{"Best Order", bestVal + "  " + bestPct + "%"})
		table.Append([]string{"Worst Order", worstVal + "  " + worstPct + "%"})
	}
	table.Append([]string{"Max Assets", strconv.FormatFloat(r.MaxReal, 'f', 1, 64)})
	table.Append([]string{"Min Assets", strconv.FormatFloat(r.MinReal, 'f', 1, 64)})
	drawDownRate := strconv.FormatFloat(r.ShowDrawDownPct, 'f', 2, 64) + "%"
	realDrawDown := strconv.FormatFloat(r.MaxDrawDownPct, 'f', 2, 64) + "%"
	table.Append([]string{"Max DrawDown", fmt.Sprintf("%v / %v", drawDownRate, realDrawDown)})
	drawDownVal := strconv.FormatFloat(r.ShowDrawDownVal, 'f', 0, 64)
	realDrawVal := strconv.FormatFloat(r.MaxDrawDownVal, 'f', 0, 64)
	table.Append([]string{"Max DrawDown", fmt.Sprintf("%v / %v", drawDownVal, realDrawVal)})
	table.Append([]string{"Max Fund Occupy", strconv.FormatFloat(r.MaxFundOccup, 'f', 0, 64)})
	table.Append([]string{"Max Occupy by Pair", strconv.FormatFloat(r.MaxOccupForPair, 'f', 0, 64)})
	sharpeStr := strconv.FormatFloat(r.SharpeRatio, 'f', 2, 64)
	sortinoStr := strconv.FormatFloat(r.SortinoRatio, 'f', 2, 64)
	table.Append([]string{"Sharpe/Sortino", sharpeStr + " / " + sortinoStr})
	table.Render()
	return b.String()
}

func (r *BTResult) groupByPairs(orders []*ormo.InOutOrder) {
	groups := groupItems(orders, true, func(od *ormo.InOutOrder, i int) string {
		return od.Symbol
	})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Sharpe > groups[j].Sharpe
	})
	r.PairGrps = groups
}

func textGroupPairs(r *BTResult) string {
	return printGroups(r.PairGrps, "Pair", true, nil, nil)
}

func (r *BTResult) groupByEnters(orders []*ormo.InOutOrder) {
	groups := groupItems(orders, true, func(od *ormo.InOutOrder, i int) string {
		return fmt.Sprintf("%s:%s", od.Strategy, od.EnterTag)
	})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Title < groups[j].Title
	})
	r.EnterGrps = groups
}

func textGroupEntTags(r *BTResult) string {
	return printGroups(r.EnterGrps, "Enter Tag", true, nil, nil)
}

func (r *BTResult) groupByExits(orders []*ormo.InOutOrder) {
	groups := groupItems(orders, true, func(od *ormo.InOutOrder, i int) string {
		return fmt.Sprintf("%s:%s", od.Strategy, od.ExitTag)
	})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Title < groups[j].Title
	})
	r.ExitGrps = groups
}

func textGroupExitTags(r *BTResult) string {
	return printGroups(r.ExitGrps, "Exit Tag", true, nil, nil)
}

func (r *BTResult) groupByProfits(orders []*ormo.InOutOrder) {
	odNum := len(orders)
	if odNum == 0 {
		return
	}
	rates := make([]float64, 0, len(orders))
	for _, od := range orders {
		rates = append(rates, od.ProfitRate)
	}
	var clsNum int
	if odNum > 150 {
		clsNum = min(19, int(math.Round(math.Pow(float64(odNum), 0.5))))
	} else {
		clsNum = int(math.Round(math.Pow(float64(odNum), 0.6)))
	}
	res := utils.KMeansVals(rates, clsNum)
	var grpTitles = make([]string, 0, len(res.Clusters))
	for _, gp := range res.Clusters {
		minPct := strconv.FormatFloat(slices.Min(gp.Items)*100, 'f', 2, 64)
		maxPct := strconv.FormatFloat(slices.Max(gp.Items)*100, 'f', 2, 64)
		grpTitles = append(grpTitles, fmt.Sprintf("%s ~ %s%%", minPct, maxPct))
	}
	groups := groupItems(orders, false, func(od *ormo.InOutOrder, i int) string {
		return grpTitles[res.RowGIds[i]]
	})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Orders[0].ProfitRate < groups[j].Orders[0].ProfitRate
	})
	r.ProfitGrps = groups
}

func textGroupProfitRanges(r *BTResult) string {
	return printGroups(r.ProfitGrps, "Profit Range", false, []string{"Enter Tags", "Exit Tags"}, makeEnterExits)
}

func (r *BTResult) groupByDates(orders []*ormo.InOutOrder) {
	units := []string{"1Y", "1Q", "1M", "1w", "1d", "6h", "1h"}
	startMS, endMS := orders[0].RealEnterMS(), orders[len(orders)-1].RealEnterMS()
	var bestTF string
	var bestTFSecs int
	var bestScore float64
	// Find the optimal granularity for grouping
	// 查找分组的最佳粒度
	for _, tf := range units {
		tfSecs := utils2.TFToSecs(tf)
		grpNum := float64(endMS-startMS) / 1000 / float64(tfSecs)
		numPerGp := float64(len(orders)) / grpNum
		score1 := utils.NearScore(grpNum, 18, 2)
		score2 := utils.NearScore(min(numPerGp, 60), 40, 1)
		curScore := score2 * score1
		if curScore > bestScore {
			bestTF = tf
			bestTFSecs = tfSecs
			bestScore = curScore
		}
	}
	if bestTF == "" {
		bestTF = "1d"
		bestTFSecs = utils2.TFToSecs(bestTF)
	}
	tfUnit := bestTF[1]
	groups := groupItems(orders, false, func(od *ormo.InOutOrder, i int) string {
		entMS := od.RealEnterMS()
		if tfUnit == 'Y' {
			return btime.ToDateStrLoc(entMS, "2006")
		} else if tfUnit == 'Q' {
			enterMS := utils2.AlignTfMSecs(entMS, int64(bestTFSecs*1000))
			return btime.ToDateStrLoc(enterMS, "2006-01")
		} else if tfUnit == 'M' {
			return btime.ToDateStrLoc(entMS, "2006-01")
		} else if tfUnit == 'd' || tfUnit == 'w' {
			enterMS := utils2.AlignTfMSecs(entMS, int64(bestTFSecs*1000))
			return btime.ToDateStrLoc(enterMS, "2006-01-02")
		} else {
			return btime.ToDateStrLoc(entMS, "2006-01-02 15")
		}
	})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Title < groups[j].Title
	})
	r.DateGrps = groups
}

func textGroupDays(r *BTResult) string {
	return printGroups(r.DateGrps, "Date", false, []string{"Enter Tags", "Exit Tags"}, makeEnterExits)
}

func makeEnterExits(orders []*ormo.InOutOrder) []string {
	enters := make(map[string]int)
	exits := make(map[string]int)
	for _, od := range orders {
		if num, ok := enters[od.EnterTag]; ok {
			enters[od.EnterTag] = num + 1
		} else {
			enters[od.EnterTag] = 1
		}
		if num, ok := exits[od.ExitTag]; ok {
			exits[od.ExitTag] = num + 1
		} else {
			exits[od.ExitTag] = 1
		}
	}
	entList := make([]string, 0, len(enters))
	exitList := make([]string, 0, len(enters))
	for k, v := range enters {
		entList = append(entList, fmt.Sprintf("%s/%v", k, v))
	}
	for k, v := range exits {
		exitList = append(exitList, fmt.Sprintf("%s/%v", k, v))
	}
	return []string{
		strings.Join(entList, " "),
		strings.Join(exitList, " "),
	}
}

func groupItems(orders []*ormo.InOutOrder, measure bool, getTag func(od *ormo.InOutOrder, i int) string) []*RowItem {
	if len(orders) == 0 {
		return nil
	}
	groups := make(map[string]*RowItem)
	for i, od := range orders {
		tag := getTag(od, i)
		sta, ok := groups[tag]
		duration := max(0, int((od.RealExitMS()-od.RealEnterMS())/1000))
		isWin := od.Profit >= 0
		if !ok {
			sta = &RowItem{
				Title: tag,
				RowPart: RowPart{
					OrderNum:     1,
					ProfitSum:    od.Profit,
					ProfitPctSum: od.ProfitRate,
					CostSum:      od.EnterCost() / od.Leverage,
					Durations:    []int{duration},
					Orders:       make([]*ormo.InOutOrder, 0, 8),
				},
			}
			sta.Orders = append(sta.Orders, od)
			if isWin {
				sta.WinCount = 1
			}
			groups[tag] = sta
		} else {
			if isWin {
				sta.WinCount += 1
			}
			sta.OrderNum += 1
			sta.ProfitSum += od.Profit
			sta.ProfitPctSum += od.ProfitRate
			sta.CostSum += od.EnterCost() / od.Leverage
			sta.Durations = append(sta.Durations, duration)
			sta.Orders = append(sta.Orders, od)
		}
	}
	if measure {
		// 分30份采样计算指标，太大的话会导致指标偏小
		for _, gp := range groups {
			sharpe, sortino, err := measurePerformance(gp.Orders)
			if err != nil {
				log.Warn("calc measure fail", zap.Error(err))
			} else {
				if !math.IsNaN(sharpe) && !math.IsInf(sharpe, 0) {
					gp.Sharpe = sharpe
				}
				if !math.IsNaN(sortino) && !math.IsInf(sortino, 0) {
					gp.Sortino = sortino
				}
			}
		}
	}
	return utils.ValsOfMap(groups)
}

func printGroups(groups []*RowItem, title string, measure bool, extHeads []string, prcGrp func([]*ormo.InOutOrder) []string) string {
	var b bytes.Buffer
	table := tablewriter.NewWriter(&b)
	heads := []string{title, "Count", "Avg Profit %", "Tot Profit %", "Sum Profit", "Duration(h'm)", "Win Rate"}
	if measure {
		heads = append(heads, "Sharpe/Sortino")
	}
	if len(extHeads) > 0 {
		heads = append(heads, extHeads...)
	}
	table.SetHeader(heads)
	table.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
	table.SetCenterSeparator("|")
	table.SetAlignment(tablewriter.ALIGN_RIGHT)
	for _, sta := range groups {
		grpCount := len(sta.Orders)
		numText := strconv.Itoa(grpCount)
		avgProfit := strconv.FormatFloat(sta.ProfitPctSum*100/float64(grpCount), 'f', 2, 64)
		totProfit := strconv.FormatFloat(sta.ProfitSum*100/sta.CostSum, 'f', 2, 64)
		sumProfit := strconv.FormatFloat(sta.ProfitSum, 'f', 2, 64)
		duraText := kMeansDurations(sta.Durations, 3)
		winRate := strconv.FormatFloat(float64(sta.WinCount)*100/float64(grpCount), 'f', 1, 64) + "%"
		row := []string{sta.Title, numText, avgProfit, totProfit, sumProfit, duraText, winRate}
		if measure {
			sharpeStr := strconv.FormatFloat(sta.Sharpe, 'f', 2, 64)
			sortinoStr := strconv.FormatFloat(sta.Sortino, 'f', 2, 64)
			row = append(row, sharpeStr+" / "+sortinoStr)
		}
		if prcGrp != nil {
			cols := prcGrp(sta.Orders)
			row = append(row, cols...)
		}
		table.Append(row)
	}
	table.Render()
	return b.String()
}

func measurePerformance(ods []*ormo.InOutOrder) (float64, float64, *errs.Error) {
	if len(ods) == 0 {
		return 0, 0, nil
	}
	sort.Slice(ods, func(i, j int) bool {
		return ods[i].RealExitMS() < ods[j].RealExitMS()
	})
	initStake := float64(0)
	for key, val := range config.WalletAmounts {
		initStake += val * core.GetPriceSafe(key)
	}
	dayTfMsecs := int64(utils2.TFToSecs("1d") * 1000)
	startMS := utils2.AlignTfMSecs(ods[0].RealEnterMS(), dayTfMsecs)
	endMS := utils2.AlignTfMSecs(ods[len(ods)-1].RealEnterMS(), dayTfMsecs) + dayTfMsecs
	returns, _, _ := ormo.CalcUnitReturns(ods, nil, startMS, endMS, dayTfMsecs)
	if len(returns) <= 2 {
		return 0, 0, nil
	}
	retRates := make([]float64, len(returns))
	for i, ret := range returns {
		retRates[i] = ret / initStake
	}
	return calcMeasures(retRates, 365)
}

func calcMeasures(returns []float64, periods int) (float64, float64, *errs.Error) {
	sharpeFlt, err := utils.SharpeRatioBy(returns, 0.02, periods, true)
	if err != nil {
		return 0, 0, errs.New(errs.CodeRunTime, err)
	}
	sortineFlt, err := utils.SortinoRatioBy(returns, 0.02, periods, true)
	if err != nil {
		if !errors.Is(err, utils.ErrNoNegativeResults) {
			return sharpeFlt, 0, errs.New(errs.CodeRunTime, err)
		}
		sortineFlt = math.Inf(1)
	}
	return sharpeFlt, sortineFlt, nil
}

func kMeansDurations(durations []int, num int) string {
	slices.Sort(durations)
	diffNum := 1
	for i, val := range durations[1:] {
		if val != durations[i] {
			diffNum += 1
		}
	}
	if diffNum < num {
		if len(durations) == 0 {
			return ""
		}
		num = diffNum
	}
	var d = make([]float64, 0, len(durations))
	for _, dura := range durations {
		d = append(d, float64(dura))
	}
	var res = utils.KMeansVals(d, num)
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, grp := range res.Clusters {
		grpNum := len(grp.Items)
		var coord int
		if grpNum == 1 {
			coord = int(math.Round(grp.Items[0]))
		} else {
			coord = int(math.Round(grp.Center))
		}
		if coord < 60 {
			b.WriteString(strconv.Itoa(coord))
			b.WriteString("s")
		} else {
			mins := coord / 60
			hours := mins / 60
			lmins := mins % 60
			b.WriteString(strconv.Itoa(hours))
			if hours <= 99 {
				b.WriteString("'")
				b.WriteString(strconv.Itoa(lmins))
			}
		}
		b.WriteString("/")
		b.WriteString(strconv.Itoa(grpNum))
		b.WriteString("  ")
	}
	return b.String()
}

func DumpOrdersCSV(orders []*ormo.InOutOrder, outPath string) error {
	sort.Slice(orders, func(i, j int) bool {
		var a, b = orders[i], orders[j]
		var ta, tb = a.RealEnterMS(), b.RealEnterMS()
		if ta != tb {
			return ta < tb
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Strategy != b.Strategy {
			return a.Strategy < b.Strategy
		}
		if a.EnterTag != b.EnterTag {
			return a.EnterTag < b.EnterTag
		}
		return a.Enter.Amount < b.Enter.Amount
	})
	file, err_ := os.Create(outPath)
	if err_ != nil {
		return err_
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	defer writer.Flush()
	heads := []string{"sid", "symbol", "timeframe", "direction", "leverage", "entAt", "entTag", "entPrice",
		"entAmount", "entCost", "entFee", "exitAt", "exitTag", "exitPrice", "exitAmount", "exitGot",
		"exitFee", "maxPftRate", "maxDrawDown", "profitRate", "profit", "strategy"}
	if err_ = writer.Write(heads); err_ != nil {
		return err_
	}
	colNum := len(heads)
	for _, od := range orders {
		row := make([]string, colNum)
		row[0] = fmt.Sprintf("%v", od.Sid)
		row[1] = od.Symbol
		row[2] = od.Timeframe
		row[3] = "long"
		if od.Short {
			row[3] = "short"
		}
		row[4] = fmt.Sprintf("%v", od.Leverage)
		row[5] = btime.ToDateStrLoc(od.RealEnterMS(), "")
		row[6] = od.EnterTag
		if od.Enter != nil {
			row[7], row[8], row[9], row[10] = calcExOrder(od.Enter)
		}
		row[11] = btime.ToDateStrLoc(od.RealExitMS(), "")
		row[12] = od.ExitTag
		if od.Exit != nil {
			row[13], row[14], row[15], row[16] = calcExOrder(od.Exit)
		}
		row[17] = strconv.FormatFloat(od.MaxPftRate, 'f', 4, 64)
		row[18] = strconv.FormatFloat(od.MaxDrawDown, 'f', 4, 64)
		row[19] = strconv.FormatFloat(od.ProfitRate, 'f', 4, 64)
		row[20] = strconv.FormatFloat(od.Profit, 'f', 8, 64)
		row[21] = od.Strategy
		if err_ = writer.Write(row); err_ != nil {
			return err_
		}
	}
	return nil
}

func calcExOrder(od *ormo.ExOrder) (string, string, string, string) {
	price := od.Average
	if price == 0 {
		price = od.Price
	}
	exitGot := price * od.Filled
	priceStr := strconv.FormatFloat(price, 'f', 8, 64)
	amtStr := strconv.FormatFloat(od.Filled, 'f', 8, 64)
	valStr := strconv.FormatFloat(exitGot, 'f', 4, 64)
	feeStr := strconv.FormatFloat(od.Fee, 'f', 8, 64)
	return priceStr, amtStr, valStr, feeStr
}

func (r *BTResult) dumpConfig() {
	data, err := config.DumpYaml(true)
	if err != nil {
		log.Error("marshal config as yaml fail", zap.Error(err))
		return
	}
	outName := fmt.Sprintf("%s/config.yml", r.OutDir)
	// 这里不检查是否存在，直接覆盖，因webUI创建的带有敏感信息
	err_ := os.WriteFile(outName, data, 0644)
	if err_ != nil {
		log.Error("save yaml to file fail", zap.Error(err_))
	}
}

func (r *BTResult) dumpStrategy() {
	stratDir := config.GetStratDir()
	if stratDir == "" {
		log.Info("env `BanStratDir` not configured, skip backup strategy")
		return
	}
	for name := range core.StgPairTfs {
		dname := strings.Split(name, ":")[0]
		curDir, err_ := utils.FindSubPath(stratDir, dname, 3)
		if err_ != nil {
			log.Warn("skip backup strat", zap.String("name", name), zap.Error(err_))
			continue
		}
		tgtDir := fmt.Sprintf("%s/strat_%s", r.OutDir, dname)
		err_ = utils.CopyDir(curDir, tgtDir)
		if err_ != nil {
			log.Warn("backup strat fail", zap.String("name", name), zap.Error(err_))
		}
	}
}

func (r *BTResult) dumpStratOutputs() {
	groups := make(map[string][]string)
	for _, items := range strat.PairStrats {
		for _, stgy := range items {
			if len(stgy.Outputs) == 0 {
				continue
			}
			rows, _ := groups[stgy.Name]
			groups[stgy.Name] = append(rows, stgy.Outputs...)
			stgy.Outputs = nil
		}
	}
	for name, rows := range groups {
		name = strings.ReplaceAll(name, ":", "_")
		outPath := fmt.Sprintf("%s/%s.log", r.OutDir, name)
		file, err := os.Create(outPath)
		if err != nil {
			log.Error("create strategy output file fail", zap.String("name", name), zap.Error(err))
			continue
		}
		_, err = file.WriteString(strings.Join(rows, "\n"))
		if err != nil {
			log.Error("write strategy output fail", zap.String("name", name), zap.Error(err))
		}
		err = file.Close()
		if err != nil {
			log.Error("close strategy output fail", zap.String("name", name), zap.Error(err))
		}
	}
}

func (r *BTResult) dumpGraph() {
	odNum := make([]float64, 0, len(r.Plots.OdNum))
	for _, v := range r.Plots.OdNum {
		odNum = append(odNum, float64(v))
	}
	jobNum := make([]float64, 0, len(r.Plots.JobNum))
	for _, v := range r.Plots.JobNum {
		jobNum = append(jobNum, float64(v))
	}
	outPath := fmt.Sprintf("%s/assets.html", r.OutDir)
	title := "Real-time Assets/Balances/Unrealized P&L/Withdrawals/Concurrent Orders"
	tplPath := fmt.Sprintf("%s/lines.html", config.GetDataDir())
	tplData, _ := os.ReadFile(tplPath)
	err := DumpChart(outPath, title, r.Plots.Labels, 5, tplData, []*ChartDs{
		{Label: "Real", Data: r.Plots.Real},
		{Label: "Available", Data: r.Plots.Available},
		{Label: "Profit", Data: r.Plots.Profit, Hidden: true},
		{Label: "UnPOL", Data: r.Plots.UnrealizedPOL, Hidden: true},
		{Label: "Withdraw", Data: r.Plots.WithDraw, Hidden: true},
		{Label: "OrderNum", Data: odNum, YAxisID: "yRight", Hidden: true},
		{Label: "JobNum", Data: jobNum, YAxisID: "yRight", Hidden: true},
	})
	if err != nil {
		log.Error("save assets.html fail", zap.Error(err))
	}
	// Draw cumulative profit curves for each entry tag separately
	// 为入场标签分别绘制累计利润曲线
	outPath = fmt.Sprintf("%s/enters.html", r.OutDir)
	err = DumpEnterTagCumProfits(outPath, ormo.HistODs, 600)
	if err != nil {
		log.Error("DumpEnterTagCumProfits fail", zap.Error(err))
	}
}

func (p *PlotData) calcDrawDown() (float64, float64) {
	var drawDownRate, maxReal, drawDownVal float64
	if len(p.Real) > 0 {
		reals := p.Real
		maxReal = reals[0]
		for _, val := range reals {
			if val > maxReal {
				maxReal = val
			} else {
				drawDownVal = max(drawDownVal, maxReal-val)
				curDown := math.Abs(val/maxReal - 1)
				if curDown > drawDownRate {
					drawDownRate = curDown
				}
			}
		}
	}
	return drawDownRate, drawDownVal
}

func (r *BTResult) calcMeasures(num int) *errs.Error {
	p := r.Plots
	if len(p.Real) <= 1 {
		return nil
	}
	step := max(1, len(p.Real)/num)
	prevVal := p.Real[0]
	inReturns := make([]float64, 0, num)
	for i := step; i < len(p.Real); i += step {
		curVal := p.Real[i]
		inReturns = append(inReturns, (curVal-prevVal)/prevVal)
		prevVal = curVal
	}
	if len(inReturns) < num {
		last := p.Real[len(p.Real)-1]
		inReturns = append(inReturns, (last-prevVal)/prevVal)
	}
	dayMSecs := int64(utils2.TFToSecs("1d") * 1000)
	// 计算一年包含交易单位数量：365 / 单个交易单位包含天数
	// 单个交易单位包含天数 = 总交易天数 / 交易单位总数
	periods := len(p.Real) * 365 / int((r.EndMS-r.StartMS)/dayMSecs)
	sharpe, sortino, err := calcMeasures(inReturns, periods)
	if err != nil {
		return err
	}
	if !math.IsNaN(sharpe) && !math.IsInf(sharpe, 0) {
		r.SharpeRatio = sharpe
	}
	if !math.IsNaN(sortino) && !math.IsInf(sortino, 0) {
		r.SortinoRatio = sortino
	}
	return nil
}

func (r *BTResult) Score() float64 {
	var score float64
	if r.TotProfitPct <= 0 {
		score = r.TotProfitPct
	} else {
		// 盈利时返回无回撤收益率
		score = r.TotProfitPct * math.Pow(1-r.ShowDrawDownPct/100, 1.5)
	}
	return utils.NanInfTo(score, 0)
}

func (r *BTResult) dumpDetail(outPath string) {
	if outPath == "" {
		outPath = fmt.Sprintf("%s/detail.json", r.OutDir)
	}
	if r.CreateMS == 0 {
		r.CreateMS = btime.UTCStamp()
	}
	data, err_ := utils2.Marshal(r)
	if err_ != nil {
		log.Error("marshal backtest detail fail", zap.Error(err_))
		return
	}
	err_ = os.WriteFile(outPath, data, 0644)
	if err_ != nil {
		log.Error("write backtest detail fail", zap.Error(err_))
	}
}

/*
DelBigObjects 删除大对象引用，避免内存泄露
*/
func (r *BTResult) DelBigObjects() {
	grpList := [][]*RowItem{r.PairGrps, r.DateGrps, r.EnterGrps, r.ExitGrps, r.ProfitGrps}
	for _, gp := range grpList {
		for _, p := range gp {
			p.Orders = nil
			p.Durations = nil
		}
	}
}

func selectPairs(r *BTResult, name string) []string {
	fn, ok := PairPickers[name]
	if !ok {
		log.Warn("PairPickers not found", zap.String("name", name))
		return nil
	}
	return fn(r)
}

func parseBtResult(path string) (*BTResult, *errs.Error) {
	data, err_ := os.ReadFile(path)
	if err_ != nil {
		return nil, errs.New(errs.CodeIOReadFail, err_)
	}
	var res = BTResult{}
	err_ = utils2.Unmarshal(data, &res, utils2.JsonNumDefault)
	if err_ != nil {
		return nil, errs.New(errs.CodeUnmarshalFail, err_)
	}
	return &res, nil
}

/*
DumpEnterTagCumProfits

export line chart of cumulative profit based on entry tag statistics

按入场信号统计累计利润导出折线图
*/
func DumpEnterTagCumProfits(path string, odList []*ormo.InOutOrder, xNum int) *errs.Error {
	labels, dsList, err := CalcGroupCumProfits(odList, func(o *ormo.InOutOrder) string {
		return fmt.Sprintf("%v:%v", o.Strategy, o.EnterTag)
	}, xNum)
	if err != nil || len(dsList) == 0 {
		return err
	}
	title := "Strategy Enter Tag Cum Profits"
	err = DumpChart(path, title, labels, 3, nil, dsList)
	return err
}

/*
CalcGroupEndProfits

calculate cumulative profit curve data for orders (directly using profit accumulation when closing positions)

生成订单累计利润曲线数据（直接使用平仓时利润累加）
*/
func CalcGroupEndProfits(odList []*ormo.InOutOrder, genKey func(o *ormo.InOutOrder) string, xNum int) ([]string, []*ChartDs) {
	if len(odList) == 0 {
		return nil, nil
	}
	startMs := odList[0].RealEnterMS()
	tagMap := make(map[string][]*TimeVal)
	for _, od := range odList {
		key := genKey(od)
		items, _ := tagMap[key]
		curVal := float64(0)
		if len(items) > 0 {
			curVal = items[len(items)-1].Value
		}
		tagMap[key] = append(items, &TimeVal{Time: od.RealExitMS(), Value: curVal + od.Profit})
	}
	endMs := odList[len(odList)-1].RealExitMS()
	gapMs := (endMs - startMs) / int64(xNum)
	var res []*ChartDs
	for tag, items := range tagMap {
		arr := make([]float64, 0, xNum+5)
		arr = append(arr, 0)
		curMs := startMs
		i := 0
		curVal := float64(0)
		next := items[i]
		for curMs+gapMs < endMs {
			curMs += gapMs
			for curMs > next.Time {
				curVal = next.Value
				if i+1 < len(items) {
					i += 1
					next = items[i]
				} else {
					next = &TimeVal{Time: math.MaxInt64}
				}
			}
			arr = append(arr, curVal)
		}
		res = append(res, &ChartDs{
			Label: tag,
			Data:  arr,
		})
	}
	curMs := startMs
	labels := make([]string, 0, len(res[0].Data))
	labels = append(labels, btime.ToDateStr(curMs, ""))
	for curMs+gapMs < endMs {
		curMs += gapMs
		labels = append(labels, btime.ToDateStr(curMs, ""))
	}
	return labels, res
}

/*
CalcGroupCumProfits

calculate cumulative profit curve data for orders (obtain K-line and calculate real-time cumulative profit for open positions)

生成订单累计利润曲线数据（获取K线，计算实时持仓累计利润）
*/
func CalcGroupCumProfits(odList []*ormo.InOutOrder, genKey func(o *ormo.InOutOrder) string, xNum int) ([]string, []*ChartDs, *errs.Error) {
	if len(odList) == 0 {
		return nil, nil, nil
	}
	groups := make(map[string]map[string][]*ormo.InOutOrder)
	minTimeMS, maxTimeMS := int64(math.MaxInt64), int64(0)
	for _, od := range odList {
		key := genKey(od)
		odMap, ok1 := groups[key]
		if !ok1 {
			odMap = make(map[string][]*ormo.InOutOrder)
			groups[key] = odMap
		}
		old, _ := odMap[od.Symbol]
		odMap[od.Symbol] = append(old, od)
		minTimeMS = min(minTimeMS, od.Enter.CreateAt)
		maxTimeMS = max(maxTimeMS, od.Exit.CreateAt)
	}
	unitSecs := int((maxTimeMS-minTimeMS)/1000) / xNum
	tf := utils.RoundSecsTF(max(unitSecs, 60))
	tfMSecs := int64(utils2.TFToSecs(tf) * 1000)
	startMS := utils2.AlignTfMSecs(minTimeMS, tfMSecs)
	endMS := utils2.AlignTfMSecs(maxTimeMS, tfMSecs) + tfMSecs
	exsMap := orm.GetExSymbolMap(core.ExgName, core.Market)
	var result []*ChartDs
	maxXNum := 0
	for key, pairMap := range groups {
		var glbRets []float64
		for pair, orders := range pairMap {
			exs := exsMap[pair]
			_, bars, err := orm.GetOHLCV(exs, tf, startMS, endMS, 0, false)
			if err != nil {
				return nil, nil, err
			}
			var closes []float64
			if len(bars) > 0 {
				bars, _ = utils.FillOHLCVLacks(bars, startMS, endMS, tfMSecs)
				closes = make([]float64, len(bars))
				for i, b := range bars {
					closes[i] = b.Close
				}
			}
			// 计算每日回报
			returns, _, _ := ormo.CalcUnitReturns(orders, closes, startMS, endMS, tfMSecs)
			if glbRets == nil {
				glbRets = returns
			} else {
				for i, v := range returns {
					glbRets[i] += v
				}
			}
		}
		// 计算累计回报
		var cumRets = make([]float64, len(glbRets))
		sumRet := float64(0)
		for i, v := range glbRets {
			sumRet += v
			cumRets[i] = sumRet
		}
		maxXNum = max(maxXNum, len(cumRets))
		result = append(result, &ChartDs{
			Label: key,
			Data:  cumRets,
		})
	}
	var labels = make([]string, 0, maxXNum)
	curMS := startMS
	lay := core.DefaultDateFmt
	if int(tfMSecs/1000) >= utils2.TFToSecs("1d") {
		lay = "2006-01-02"
	}
	for i := 0; i < maxXNum; i++ {
		dateStr := btime.ToDateStr(curMS, lay)
		labels = append(labels, dateStr)
		curMS += tfMSecs
	}
	return labels, result, nil
}

type TimeVal struct {
	Time  int64
	Value float64
}

/*
SampleOdNums
对一系列订单在整个时间范围的每个时间节点采样计算
*/
func SampleOdNums(odList []*ormo.InOutOrder, num int) ([]int, int64, int64) {
	if len(odList) == 0 {
		return make([]int, num), 0, 0
	}
	sort.Slice(odList, func(i, j int) bool {
		return odList[i].RealEnterMS() < odList[j].RealEnterMS()
	})
	nums := make([]int, num)
	minTimeMS, maxTimeMS := odList[0].RealEnterMS(), int64(0)
	for _, od := range odList {
		maxTimeMS = max(od.RealExitMS(), maxTimeMS)
	}
	gapMS := (maxTimeMS - minTimeMS) / int64(num)
	for _, od := range odList {
		startIdx := int((od.RealEnterMS() - minTimeMS) / gapMS)
		endPos := len(nums)
		if od.RealExitMS() > 0 {
			endPos = int(math.Ceil(float64(od.RealExitMS()-minTimeMS)/float64(gapMS))) + 1
			endPos = min(len(nums), endPos)
		}
		for i := startIdx; i < endPos; i++ {
			nums[i] += 1
		}
	}
	return nums, minTimeMS, maxTimeMS
}
