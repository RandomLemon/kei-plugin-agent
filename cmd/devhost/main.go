// Command devhost 是本插件的本地联调宿主：装配 kei、平台适配器与本插件后运行。
//
// 它与 kei 的 cmd/bot 同构，空导入本插件与所需适配器；连哪个 bot、用哪个数据库
// 全部由 config.yaml 与环境变量（.env）决定，见 docs/testing.md §11.5。
// 新增平台时在下方补一行适配器空导入即可。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/RandomLemon/kei/pkg/kei"

	_ "github.com/RandomLemon/kei-plugin-persona"
	_ "github.com/RandomLemon/kei/adapters/mock"
	_ "github.com/RandomLemon/kei/adapters/onebot"
	// SQL 存储方言（sqlite/mysql），使 storage.type 可选持久后端；sqlite 需要 cgo。
	_ "github.com/RandomLemon/kei/pkg/kei/sql"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := kei.Run(ctx, kei.Options{ConfigFile: *configPath}); err != nil {
		fmt.Fprintln(os.Stderr, "devhost:", err)
		os.Exit(1)
	}
}
