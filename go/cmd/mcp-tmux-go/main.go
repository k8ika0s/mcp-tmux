package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/k8ika0s/mcp-tmux/go/internal/server"
	tmuxproto "github.com/k8ika0s/mcp-tmux/go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func main() {
	addr := flag.String("listen", ":9000", "gRPC listen address")
	tmuxBin := flag.String("tmux", "tmux", "tmux binary")
	pathAdd := flag.String("path-add", "/opt/homebrew/bin:/usr/local/bin:/usr/bin", "extra PATH entries (colon-separated)")
	enableReflection := flag.Bool("reflection", true, "enable gRPC reflection")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	svc := server.NewService(*tmuxBin, strings.Split(*pathAdd, ":"))
	tmuxproto.RegisterTmuxServiceServer(grpcServer, svc)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	if *enableReflection {
		reflection.Register(grpcServer)
	}

	fmt.Printf("tmux gRPC server listening on %s (tmux=%s)\n", *addr, *tmuxBin)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
