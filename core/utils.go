package core

import (
	"fmt"
	"github.com/banbox/banexg/log"
	"regexp"
	"slices"
	"strings"
)

/*
GroupByPairQuotes
将[key]:pairs...输出为下面字符串
【key】
Quote: Base1 Base2 ...
*/
func GroupByPairQuotes(items map[string][]string) string {
	res := make(map[string]map[string][]string)
	for key, arr := range items {
		slices.Sort(arr)
		quoteMap := make(map[string][]string)
		for _, pair := range arr {
			baseCode, quoteCode, _, _ := SplitSymbol(pair)
			baseList, _ := quoteMap[quoteCode]
			quoteMap[quoteCode] = append(baseList, baseCode)
		}
		for quote, baseList := range quoteMap {
			slices.Sort(baseList)
			quoteMap[quote] = baseList
		}
		res[key] = quoteMap
	}
	var b strings.Builder
	for key, quoteMap := range res {
		b.WriteString(fmt.Sprintf("【%s】\n", key))
		for quoteCode, arr := range quoteMap {
			baseStr := strings.Join(arr, " ")
			b.WriteString(fmt.Sprintf("%s(%d): %s\n", quoteCode, len(arr), baseStr))
		}
	}
	return b.String()
}

/*
PrintStagyGroups
从core.StgPairTfs输出策略+时间周期的币种信息到控制台
*/
func PrintStagyGroups() {
	groups := make(map[string][]string)
	for stagy, pairMap := range StgPairTfs {
		for pair, tf := range pairMap {
			key := fmt.Sprintf("%s_%s", stagy, tf)
			arr, _ := groups[key]
			groups[key] = append(arr, pair)
		}
	}
	text := GroupByPairQuotes(groups)
	log.Info("group pairs by stagy_tf:\n" + text)
}

var (
	reCoinSplit = regexp.MustCompile("[/:-]")
)

/*
SplitSymbol
返回：Base，Quote，Settle，Identifier
*/
func SplitSymbol(pair string) (string, string, string, string) {
	parts := reCoinSplit.Split(pair, -1)
	settle, ident := "", ""
	if len(parts) > 2 {
		settle = parts[2]
	}
	if len(parts) > 3 {
		ident = parts[3]
	}
	return parts[0], parts[1], settle, ident
}
