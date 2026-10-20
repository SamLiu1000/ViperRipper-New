// lint 静态体检 host 规则目录：go run ./cmd/lint build/bin/data/hosts
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"viperipper/internal/host"
)

func main() {
	dir := "data/hosts"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	abs, _ := filepath.Abs(dir)
	reg := host.NewRegistry(filepath.Dir(abs))
	if err := reg.SetHostsDir(abs); err != nil {
		fmt.Fprintln(os.Stderr, "读取目录失败：", err)
		os.Exit(1)
	}
	rules := reg.List()
	issues := reg.Lint()
	errs := 0
	for _, is := range issues {
		tag := "warn "
		if is.Level == "error" {
			tag = "ERROR"
			errs++
		}
		where := is.Rule
		if is.Step > 0 {
			where = fmt.Sprintf("%s#%d", is.Rule, is.Step)
		}
		fmt.Printf("[%s] %-28s %s\n", tag, where, is.Message)
	}
	fmt.Printf("\n目录：%s\n规则：%d 条，错误 %d，提醒 %d\n", abs, len(rules), errs, len(issues)-errs)
	if errs > 0 {
		os.Exit(2)
	}
}
