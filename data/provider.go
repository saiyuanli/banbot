package data

import (
	"fmt"
	"github.com/banbox/banbot/btime"
	"github.com/banbox/banbot/config"
	"github.com/banbox/banbot/core"
	"github.com/banbox/banbot/exg"
	"github.com/banbox/banbot/orm"
	"github.com/banbox/banbot/utils"
	"github.com/banbox/banexg"
	"github.com/banbox/banexg/errs"
	"github.com/banbox/banexg/log"
	"go.uber.org/zap"
	"math"
	"sort"
	"sync"
)

type IProvider interface {
	LoopMain() *errs.Error
	SubWarmPairs(items map[string]map[string]int, delOther bool) *errs.Error
	UnSubPairs(pairs ...string) *errs.Error
	SetDirty()
}

type Provider[T IKlineFeeder] struct {
	holders   map[string]T
	newFeeder func(pair string, tfs []string) (T, *errs.Error)
	dirtyVers chan int
	dirtyLast int
}

func (p *Provider[IKlineFeeder]) UnSubPairs(pairs ...string) []string {
	var removed []string
	for _, pair := range pairs {
		if _, ok := p.holders[pair]; ok {
			delete(p.holders, pair)
			removed = append(removed, pair)
		}
	}
	return removed
}

func (p *Provider[IKlineFeeder]) SetDirty() {
	p.dirtyLast += 1
	p.dirtyVers <- p.dirtyLast
}

type WarmJob struct {
	hold    IKlineFeeder
	tfWarms map[string]int
}

/*
SubWarmPairs
Add new trading pair subscription from data provider.

items: pair[timeFrame]warmNum
Return the trading pairs with the smallest period change (new/old pairs new period), warm-up tasks
从数据提供者添加新的交易对订阅。

	items: pair[timeFrame]warmNum
	返回最小周期变化的交易对(新增/旧对新周期)、预热任务
*/
func (p *Provider[IKlineFeeder]) SubWarmPairs(items map[string]map[string]int, delOther bool) ([]IKlineFeeder, map[string]int64, []string, *errs.Error) {
	var newHolds []IKlineFeeder
	var warmJobs []*WarmJob
	var oldSince = make(map[string]int64)
	var err *errs.Error
	for pair, tfWarms := range items {
		hold, ok := p.holders[pair]
		if !ok {
			hold, err = p.newFeeder(pair, utils.KeysOfMap(tfWarms))
			if err != nil {
				return nil, nil, nil, err
			}
			p.holders[pair] = hold
			newHolds = append(newHolds, hold)
			warmJobs = append(warmJobs, &WarmJob{hold: hold, tfWarms: tfWarms})
		} else {
			oldMinTf := hold.getStates()[0].TimeFrame
			newTfs := hold.SubTfs(utils.KeysOfMap(tfWarms), delOther)
			curMinTf := hold.getStates()[0].TimeFrame
			if oldMinTf != curMinTf {
				newHolds = append(newHolds, hold)
			} else {
				oldSince[fmt.Sprintf("%s|%s", pair, curMinTf)] = hold.getStates()[0].NextMS
			}
			if len(newTfs) > 0 {
				warmJobs = append(warmJobs, &WarmJob{
					hold:    hold,
					tfWarms: utils.CutMap(tfWarms, newTfs...),
				})
			}
		}
	}
	var delPairs []string
	if delOther {
		for pair := range p.holders {
			if _, ok := items[pair]; !ok {
				delete(p.holders, pair)
				delPairs = append(delPairs, pair)
			}
		}
	}
	// 加载数据预热
	sinceMap, err := p.warmJobs(warmJobs)
	for key, since := range oldSince {
		sinceMap[key] = since
	}
	return newHolds, sinceMap, delPairs, err
}

func (p *Provider[IKlineFeeder]) warmJobs(warmJobs []*WarmJob) (map[string]int64, *errs.Error) {
	sinceMap := make(map[string]int64)
	lockMap := sync.Mutex{}
	jobNum := 0
	// 预热所需的必要数据
	for _, job := range warmJobs {
		jobNum += len(job.tfWarms)
	}
	log.Info(fmt.Sprintf("warmup for %d pairs, %v jobs", len(warmJobs), jobNum))
	pBar := utils.NewPrgBar(jobNum*core.StepTotal, "warmup")
	defer pBar.Close()
	startTime := btime.TimeMS()
	retErr := utils.ParallelRun(warmJobs, core.ConcurNum, func(_ int, job *WarmJob) *errs.Error {
		hold := job.hold
		since, err := hold.WarmTfs(startTime, job.tfWarms, pBar)
		lockMap.Lock()
		sinceMap[hold.getSymbol()] = since
		lockMap.Unlock()
		return err
	})
	return sinceMap, retErr
}

type HistProvider struct {
	Provider[IHistKlineFeeder]
}

func NewHistProvider(callBack FnPairKline, envEnd FuncEnvEnd) *HistProvider {
	return &HistProvider{
		Provider: Provider[IHistKlineFeeder]{
			holders: make(map[string]IHistKlineFeeder),
			newFeeder: func(pair string, tfs []string) (IHistKlineFeeder, *errs.Error) {
				exs, err := orm.GetExSymbolCur(pair)
				if err != nil {
					return nil, err
				}
				feeder, err := NewDBKlineFeeder(exs, callBack)
				if err != nil {
					return nil, err
				}
				feeder.OnEnvEnd = envEnd
				feeder.SubTfs(tfs, false)
				return feeder, nil
			},
			dirtyVers: make(chan int, 5),
		},
	}
}

func (p *HistProvider) downIfNeed() *errs.Error {
	exchange := exg.Default
	if !exchange.HasApi(banexg.ApiFetchOHLCV, core.Market) {
		return nil
	}
	var err *errs.Error
	sess, conn, err := orm.Conn(nil)
	if err != nil {
		return err
	}
	defer conn.Release()
	pBar := utils.NewPrgBar(len(p.holders)*core.StepTotal, "DownHist")
	defer pBar.Close()
	for _, h := range p.holders {
		err = h.DownIfNeed(sess, exchange, pBar)
		if err != nil {
			log.Error("download ohlcv fail", zap.String("pair", h.getSymbol()), zap.Error(err))
			return err
		}
	}
	return nil
}

func (p *HistProvider) SubWarmPairs(items map[string]map[string]int, delOther bool) *errs.Error {
	_, sinceMap, _, err := p.Provider.SubWarmPairs(items, delOther)
	// Check whether the data needs to be downloaded during the backtest. If so, it will be downloaded automatically.
	// 检查回测期间数据是否需要下载，如需要自动下载
	err = p.downIfNeed()
	if err != nil {
		return err
	}
	maxSince := int64(0)
	holders := make(map[string]IHistKlineFeeder)
	for pair, since := range sinceMap {
		hold, ok := p.holders[pair]
		if !ok {
			continue
		}
		holders[pair] = hold
		if hold.getNextMS() == 0 || hold.getStates()[0].NextMS != since {
			// Ignore here the targets that still exist after refreshing the trading pairs.
			// 这里忽略刷新交易对后，仍然存在的标的
			hold.SetSeek(since)
		}
		maxSince = max(maxSince, since)
	}
	// Delete items that are not warmed up
	// 删除未预热的项
	p.holders = holders
	btime.CurTimeMS = maxSince
	return err
}

func (p *HistProvider) UnSubPairs(pairs ...string) *errs.Error {
	_ = p.Provider.UnSubPairs(pairs...)
	return nil
}

func (p *HistProvider) LoopMain() *errs.Error {
	if len(p.holders) == 0 {
		return errs.NewMsg(core.ErrBadConfig, "no pairs to run")
	}
	makeFeeders := func() []IHistKlineFeeder {
		return utils.ValsOfMap(p.holders)
	}
	totalMS := (config.TimeRange.EndMS - config.TimeRange.StartMS) / 1000
	var pBar = utils.NewPrgBar(int(totalMS), "RunHist")
	defer pBar.Close()
	pBar.Last = config.TimeRange.StartMS
	log.Info("run data loop for backtest..")
	return RunHistFeeders(makeFeeders, p.dirtyVers, pBar)
}

func (p *HistProvider) Terminate() {
	p.dirtyVers <- -1
}

/*
RunHistFeeders run hist feeders for historical data

versions: When an integer greater than the previous value is received, makeFeeders will be called to re-acquire and continue running; when a negative number is received, exit immediately

pBar: optional, used to display a progress bar
*/
func RunHistFeeders(makeFeeders func() []IHistKlineFeeder, versions chan int, pBar *utils.PrgBar) *errs.Error {
	var hold IHistKlineFeeder
	var wg sync.WaitGroup
	var retErr *errs.Error
	var lastBarMs int64
	var oldVer int
	var holds []IHistKlineFeeder
	step := func(ver int) bool {
		if ver > oldVer || len(holds) == 0 {
			holds = makeFeeders()
			holds = SortFeeders(holds, nil, false)
			oldVer = max(oldVer, ver)
		} else {
			holds = SortFeeders(holds, hold, true)
		}
		hold = holds[0]
		bar := hold.GetBar()
		if bar == nil {
			return false
		}
		hold.CallNext()
		holds = holds[1:]
		if bar.Time > lastBarMs {
			// Wait for concurrent batches to complete
			// 等待并发批量完成
			wg.Wait()
			if retErr != nil {
				log.Error("data loop main fail", zap.Error(retErr))
				return false
			}
			// 更新进度条
			if pBar != nil {
				if pBar.Last == 0 {
					pBar.Last = btime.TimeMS()
				} else {
					pBarAdd := (btime.TimeMS() - pBar.Last) / 1000
					if pBarAdd > 0 {
						pBar.Add(int(pBarAdd))
						pBar.Last = btime.TimeMS()
					}
				}
			}
			// The first bar of the new time, run synchronously
			// 新时间的第一个bar，同步运行
			err := hold.RunBar(bar)
			if err != nil {
				log.Error("data loop main fail", zap.Error(err))
				return false
			}
			lastBarMs = bar.Time
		} else {
			wg.Add(1)
			// Different targets at the same time, executed concurrently
			// 同一个时间的不同标的，并发执行
			go func(hold IHistKlineFeeder, bar *banexg.Kline) {
				defer wg.Done()
				if retErr != nil {
					return
				}
				err := hold.RunBar(bar)
				if err != nil {
					retErr = err
				}
			}(hold, bar)
		}
		return true
	}
	for {
		var ver = 0
		select {
		case ver = <-versions:
			if ver < 0 {
				return retErr
			}
		default:
			ver = 0
		}
		if !step(ver) {
			break
		}
	}
	wg.Wait()
	return retErr
}

func SortFeeders(holds []IHistKlineFeeder, hold IHistKlineFeeder, insert bool) []IHistKlineFeeder {
	if insert {
		// 插入排序，说明holds已有序，二分查找位置，最快排序
		vb := hold.getNextMS()
		bSymbol := hold.getSymbol()
		index := sort.Search(len(holds), func(i int) bool {
			va := holds[i].getNextMS()
			if va == math.MaxInt64 || vb == math.MaxInt64 {
				return va > vb
			}
			if va != vb {
				return va > vb
			}
			return holds[i].getSymbol() > bSymbol
		})
		holds = append(holds, hold)
		copy(holds[index+1:], holds[index:])
		holds[index] = hold
		return holds
	}
	sort.Slice(holds, func(i, j int) bool {
		a, b := holds[i], holds[j]
		va, vb := a.getNextMS(), b.getNextMS()
		if va == math.MaxInt64 || vb == math.MaxInt64 {
			return va < vb
		}
		if va != vb {
			return va < vb
		}
		return a.getSymbol() < b.getSymbol()
	})
	return holds
}

type LiveProvider struct {
	Provider[IKlineFeeder]
	*KLineWatcher
}

func NewLiveProvider(callBack FnPairKline, envEnd FuncEnvEnd) (*LiveProvider, *errs.Error) {
	watcher, err := NewKlineWatcher(config.SpiderAddr)
	if err != nil {
		return nil, err
	}
	provider := &LiveProvider{
		Provider: Provider[IKlineFeeder]{
			holders: make(map[string]IKlineFeeder),
			newFeeder: func(pair string, tfs []string) (IKlineFeeder, *errs.Error) {
				exs, err := orm.GetExSymbol(exg.Default, pair)
				if err != nil {
					return nil, err
				}
				feeder, err := NewKlineFeeder(exs, callBack)
				if err != nil {
					return nil, err
				}
				feeder.SubTfs(tfs, false)
				feeder.OnEnvEnd = envEnd
				return feeder, nil
			},
			dirtyVers: make(chan int, 5),
		},
		KLineWatcher: watcher,
	}
	watcher.OnKLineMsg = makeOnKlineMsg(provider)
	// 立刻订阅实时价格
	err = watcher.SendMsg("subscribe", []string{
		fmt.Sprintf("price_%s_%s", core.ExgName, core.Market),
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *LiveProvider) SubWarmPairs(items map[string]map[string]int, delOther bool) *errs.Error {
	newHolds, sinceMap, delPairs, err := p.Provider.SubWarmPairs(items, delOther)
	if err != nil {
		return err
	}
	if len(newHolds) > 0 {
		var jobs []WatchJob
		for _, h := range newHolds {
			symbol, timeFrame := h.getSymbol(), h.getStates()[0].TimeFrame
			if since, ok := sinceMap[symbol]; ok {
				jobs = append(jobs, WatchJob{
					Symbol:    symbol,
					TimeFrame: timeFrame,
					Since:     since,
				})
			}
		}
		err = p.WatchJobs(core.ExgName, core.Market, "ohlcv", jobs...)
		if err != nil {
			return err
		}
		if len(core.BookPairs) > 0 {
			jobs = make([]WatchJob, 0, len(core.BookPairs))
			for pair := range core.BookPairs {
				jobs = append(jobs, WatchJob{Symbol: pair, TimeFrame: "1m"})
			}
			err = p.WatchJobs(core.ExgName, core.Market, "book", jobs...)
			if err != nil {
				return err
			}
		}
	}
	if len(delPairs) > 0 {
		err = p.UnWatchJobs(core.ExgName, core.Market, "ohlcv", delPairs)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *LiveProvider) UnSubPairs(pairs ...string) *errs.Error {
	removed := p.Provider.UnSubPairs(pairs...)
	if len(removed) == 0 {
		return nil
	}
	return p.UnWatchJobs(core.ExgName, core.Market, "ohlcv", pairs)
}

func (p *LiveProvider) LoopMain() *errs.Error {
	return p.RunForever()
}

func makeOnKlineMsg(p *LiveProvider) func(msg *KLineMsg) {
	return func(msg *KLineMsg) {
		if msg.ExgName != core.ExgName || msg.Market != core.Market {
			return
		}
		hold, ok := p.holders[msg.Pair]
		if !ok {
			return
		}
		tfMSecs := int64(msg.TFSecs * 1000)
		// The weighting factor has been calculated during the start-up or market break, and the weighting is automatically carried out internally
		// 已在启动或休市期间计算复权因子，内部会自动进行复权
		if msg.Interval >= msg.TFSecs {
			_, err := hold.onNewBars(tfMSecs, msg.Arr)
			if err != nil {
				log.Error("onNewBars fail", zap.String("p", msg.Pair), zap.Error(err))
			}
			return
		}
		// The frequency of updates is lower than the bar cycle, and what is received may not be completed
		// 更新频率低于bar周期，收到的可能未完成
		lastIdx := len(msg.Arr) - 1
		doneArr, lastBar := msg.Arr[:lastIdx], msg.Arr[lastIdx]
		waitBar := hold.getWaitBar()
		if waitBar != nil && waitBar.Time < lastBar.Time {
			doneArr = append([]*banexg.Kline{waitBar}, doneArr...)
			hold.setWaitBar(nil)
		}
		if len(doneArr) > 0 {
			_, err := hold.onNewBars(tfMSecs, doneArr)
			if err != nil {
				log.Error("onNewBars fail", zap.String("p", msg.Pair), zap.Error(err))
			}
			return
		}
		if msg.Interval <= 5 && hold.getStates()[0].TFSecs >= 60 {
			// The update is fast, and the cycle required is relatively long, so it is required to be considered complete when the next bar occurs (follow the above logic)
			// 更新很快，需要的周期相对较长，则要求出现下一个bar时认为完成（走上面逻辑）
			hold.setWaitBar(lastBar)
			return
		}
		// The frequency of updates is relatively low, or the proportion of the required cycle is large, and the approximate completion is considered complete
		// 更新频率相对不高，或占需要的周期比率较大，近似完成认为完成
		endLackSecs := int((lastBar.Time + tfMSecs - btime.TimeMS()) / 1000)
		if endLackSecs*2 < msg.Interval {
			// The missing time is less than half of the update interval and is considered complete.
			// 缺少的时间不足更新间隔的一半，认为完成。
			_, err := hold.onNewBars(tfMSecs, []*banexg.Kline{lastBar})
			if err != nil {
				log.Error("onNewBars fail", zap.String("p", msg.Pair), zap.Error(err))
			}
		} else {
			hold.setWaitBar(lastBar)
		}
	}
}
