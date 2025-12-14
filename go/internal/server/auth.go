package server

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// authFromContext extracts bearer or x-mcp-token.
func authFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get("x-mcp-token"); len(vals) > 0 {
		return vals[0]
	}
	if vals := md.Get("authorization"); len(vals) > 0 {
		v := vals[0]
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			return strings.TrimSpace(v[7:])
		}
		return v
	}
	return ""
}

func unaryAuthInterceptor(expected string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if expected != "" && authFromContext(ctx) != expected {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing token")
		}
		return handler(ctx, req)
	}
}

func streamAuthInterceptor(expected string) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if expected != "" && authFromContext(ss.Context()) != expected {
			return status.Error(codes.Unauthenticated, "invalid or missing token")
		}
		return handler(srv, ss)
	}
}

// AuthOptions returns grpc.ServerOption with auth interceptors when token is set.
func AuthOptions(token string) []grpc.ServerOption {
	if token == "" {
		return nil
	}
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(unaryAuthInterceptor(token)),
		grpc.StreamInterceptor(streamAuthInterceptor(token)),
	}
}
