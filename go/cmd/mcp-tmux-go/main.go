package main

import (
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/k8ika0s/mcp-tmux/go/internal/server"
	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"google.golang.org/grpc"
)

func main() {
	addr := flag.String("listen", ":9000", "gRPC listen address")
	tmuxBin := flag.String("tmux", "tmux", "tmux binary")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	svc := server.NewService(*tmuxBin, []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"})
	tmuxproto.RegisterTmuxServiceServer(grpcServer, svc)

	fmt.Printf("tmux gRPC server listening on %s (tmux=%s)\n", *addr, *tmuxBin)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
