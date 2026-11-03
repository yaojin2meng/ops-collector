// ops-collector：new-api / sub2api 多站点运营数据采集器（Go 版）。
package main

import (
	"os"

	"opscollector/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
