package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"icloud-alias-manager/internal/apple"
	"icloud-alias-manager/internal/engine"
	"icloud-alias-manager/internal/store"
	"icloud-alias-manager/internal/web"
)

func main() {
	port := flag.Int("port", 8788, "HTTP 服务监听端口")
	host := flag.String("host", "0.0.0.0", "HTTP 服务监听地址")
	dataDir := flag.String("data", "data", "数据与存储保存目录")
	flag.Parse()

	fmt.Println("=======================================================")
	fmt.Println("       iCloud 别名批量创建与管理系统 (HME Manager)       ")
	fmt.Println("=======================================================")

	// Initialize persistent store
	st, err := store.NewStore(*dataDir)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}

	// Initialize apple auth client
	authClient := apple.NewAuthClient()

	// Initialize runner engine
	runner := engine.NewRunner(st, authClient)

	// Initialize HTTP server
	srv := web.NewServer(st, runner, authClient)

	addr := fmt.Sprintf("%s:%d", *host, *port)
	fmt.Printf("[启动成功] 控制台已就绪，请在浏览器打开:\n")
	fmt.Printf("           👉 http://127.0.0.1:%d\n", *port)
	fmt.Printf("           👉 http://localhost:%d\n", *port)
	fmt.Println("=======================================================")

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		fmt.Println("\n正在停止后台任务并保存数据...")
		runner.Stop()
		_ = st.Save()
		os.Exit(0)
	}()

	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatalf("HTTP 服务运行出错: %v", err)
	}
}
