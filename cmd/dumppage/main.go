// 一次性调试工具:抓一个主题页并按楼层切分,验证 HTML 解析结果。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"viperipper/internal/forum"
	"viperipper/internal/proxy"
)

func main() {
	threadID := int64(16426403)
	if len(os.Args) > 1 {
		fmt.Sscanf(os.Args[1], "%d", &threadID)
	}
	_ = proxy.Func()
	c := forum.NewClient("https://viper.click/vr.php", "vipergirls.to", 2)
	if raw, err := os.ReadFile("build/bin/data/_cookies.json"); err == nil {
		var cookies []*http.Cookie
		if json.Unmarshal(raw, &cookies) == nil {
			if err := c.ImportCookies(cookies); err != nil {
				fmt.Println("cookie import err:", err)
			} else {
				user, _ := c.CheckLogin()
				fmt.Println("login as:", user)
			}
		}
	}
	page, next, err := c.FetchPage(context.Background(), c.ThreadPageURL(threadID))
	if err != nil {
		fmt.Println("fetch err:", err)
		return
	}
	os.WriteFile("build/bin/data/_threadpage.html", []byte(page), 0644)
	fmt.Println("page bytes:", len(page), "next:", next)
	floors := c.ParseFloorPage(page)
	fmt.Println("floors:", len(floors))
	for _, f := range floors {
		fmt.Printf("  post %d no=%d imgs=%d text=%.40s\n", f.ID, f.FloorNo, len(f.Images), strings.ReplaceAll(f.Text, "\n", " "))
	}
}
