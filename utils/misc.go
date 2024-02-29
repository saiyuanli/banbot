package utils

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	regHolds, _ = regexp.Compile("[{]([^}]+)[}]")
)

/*
SplitSolid 字符串分割，忽略返回结果中的空字符串
*/
func SplitSolid(text string, sep string) []string {
	arr := strings.Split(text, sep)
	var result []string
	for _, str := range arr {
		if str != "" {
			result = append(result, str)
		}
	}
	return result
}

func KeysOfMap[M ~map[K]V, K comparable, V any](m M) []K {
	r := make([]K, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	return r
}

func ValsOfMap[M ~map[K]V, K comparable, V any](m M) []V {
	r := make([]V, 0, len(m))
	for _, v := range m {
		r = append(r, v)
	}
	return r
}

func CutMap[M ~map[K]V, K comparable, V any](m M, keys ...K) M {
	r := make(M)
	for _, k := range keys {
		if v, ok := m[k]; ok {
			r[k] = v
		}
	}
	return r
}

func Check(err error) {
	if err != nil {
		panic(err)
	}
}

func UnionArr[T comparable](arrs ...[]T) []T {
	hit := make(map[T]struct{})
	for _, arr := range arrs {
		for _, text := range arr {
			hit[text] = struct{}{}
		}
	}
	result := make([]T, 0, len(hit))
	for key := range hit {
		result = append(result, key)
	}
	return result
}

func ConvertArr[T1, T2 any](arr []T1, doMap func(T1) T2) []T2 {
	var res = make([]T2, len(arr))
	for i, item := range arr {
		res[i] = doMap(item)
	}
	return res
}

func ArrToMap[T1 comparable, T2 any](arr []T2, doMap func(T2) T1) map[T1][]T2 {
	res := make(map[T1][]T2)
	for _, v := range arr {
		key := doMap(v)
		if old, ok := res[key]; ok {
			res[key] = append(old, v)
		} else {
			res[key] = []T2{v}
		}
	}
	return res
}

func RemoveFromArr[T comparable](arr []T, it T, num int) []T {
	res := make([]T, 0, len(arr))
	for _, v := range arr {
		if v == it && (num < 0 || num > 0) {
			num -= 1
			continue
		}
		res = append(res, v)
	}
	return res
}

func FormatWithMap(text string, args map[string]interface{}) string {
	var b strings.Builder
	matches := regHolds.FindAllStringSubmatchIndex(text, -1)
	var lastEnd int
	for _, mat := range matches {
		start, end := mat[2], mat[3]
		b.WriteString(text[lastEnd : start-1])
		holdText := text[start:end]
		parts := strings.Split(holdText, ":")
		valFmt := "%v"
		if len(parts) > 1 && len(parts[1]) > 0 {
			valFmt = "%" + parts[1]
		}
		if val, ok := args[parts[0]]; ok {
			b.WriteString(fmt.Sprintf(valFmt, val))
		}
		lastEnd = end + 1
	}
	b.WriteString(text[lastEnd:])
	return b.String()
}
