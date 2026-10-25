// Command dsh-go serves the DeepSeek Harness web client with a Go backend.
package main

import (
	"os"

	"github.com/xhd2015/deepseek-harness-go/run"
)

func main() {
	run.Main(os.Args[1:])
}
