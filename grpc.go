// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package trace

import (
	"strings"

	"golang.org/x/net/context"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"log"
)

const grpcMetadataKey = "x-cloud-trace-context"

// GRPCClientInterceptor returns a grpc.UnaryClientInterceptor that traces all outgoing requests from a gRPC client.
// The calling context should already have a *trace.Span; a child span will be
// created for the outgoing gRPC call. If the calling context doesn't have a span,
// the call will not be traced.
//
// The functionality in gRPC that this feature relies on is currently experimental.
func GRPCClientInterceptor() grpc.UnaryClientInterceptor {
	return grpc.UnaryClientInterceptor(grpcUnaryInterceptor)
}

func grpcUnaryInterceptor(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	// TODO: also intercept streams.
	span := FromContext(ctx).NewChild(method)
	defer span.Finish()

	if span != nil {
		header := spanHeader(span.trace.traceID, span.span.ParentSpanId, span.trace.globalOptions)
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.Pairs(grpcMetadataKey, header)
		} else {
			md = md.Copy() // metadata is immutable, copy.
			md[grpcMetadataKey] = []string{header}
		}
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	err := invoker(ctx, method, req, reply, cc, opts...)
	if err != nil {
		// TODO: standardize gRPC label names?
		span.SetLabel("error", err.Error())
	}
	return err
}

// GRPCServerInterceptor returns a grpc.UnaryServerInterceptor that enables the tracing of the incoming
// gRPC calls. Incoming call's context can be used to extract the span on servers that enabled this option:
//
//	span := trace.FromContext(ctx)
//
// The functionality in gRPC that this feature relies on is currently experimental.
func GRPCServerInterceptor(tc *Client) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if header, ok := md[grpcMetadataKey]; ok {
			span := tc.SpanFromHeader("", strings.Join(header, ""))
			defer span.Finish()
			ctx = NewContext(ctx, span)
		}
		return handler(ctx, req)
	}
}

// EnableGRPCTracing automatically traces all outgoing gRPC calls from cloud.google.com/go clients.
//
// The functionality in gRPC that this relies on is currently experimental.
//
// Deprecated: Use option.WithGRPCDialOption(grpc.WithUnaryInterceptor(GRPCClientInterceptor())) instead.
var EnableGRPCTracing option.ClientOption = option.WithGRPCDialOption(grpc.WithUnaryInterceptor(GRPCClientInterceptor()))

type ClientStreamWrapper struct {
	stream grpc.ClientStream
	span   *Span
}

func (s *ClientStreamWrapper) Header() (metadata.MD, error) {
	return s.stream.Header()
}

func (s *ClientStreamWrapper) Trailer() metadata.MD {
	return s.stream.Trailer()
}

func (s *ClientStreamWrapper) CloseSend() error {
	if s.span != nil {
		s.span.Finish()
	}
	return s.stream.CloseSend()
}

func (s *ClientStreamWrapper) Context() context.Context {
	return s.stream.Context()
}

func (s *ClientStreamWrapper) SendMsg(m interface{}) error {
	err := s.stream.SendMsg(m)
	if err != nil && s.span != nil {
		s.span.Finish()
	}
	return err
}

func (s *ClientStreamWrapper) RecvMsg(m interface{}) error {
	err := s.stream.RecvMsg(m)
	if err != nil && s.span != nil {
		s.span.Finish()
	}
	return err
}

func GRPCStreamClientInterceptor() grpc.StreamClientInterceptor {
	return grpc.StreamClientInterceptor(grpcStreamClientInterceptor)
}

func grpcStreamClientInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
	streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {

	span := FromContext(ctx).NewChild(method)

	if span != nil {
		header := spanHeader(span.trace.traceID, span.span.ParentSpanId, span.trace.globalOptions)
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.Pairs(grpcMetadataKey, header)
		} else {
			md = md.Copy() // metadata is immutable, copy.
			md[grpcMetadataKey] = []string{header}
		}
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	cs, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		span.Finish()
		return nil, err
	}
	return &ClientStreamWrapper{stream: cs, span: span}, nil
}

type ServerStreamWrapper struct {
	stream  grpc.ServerStream
	span    *Span
	context context.Context
}

func (s *ServerStreamWrapper) SetHeader(md metadata.MD) error {
	return s.stream.SetHeader(md)
}

func (s *ServerStreamWrapper) SendHeader(md metadata.MD) error {
	return s.stream.SendHeader(md)
}

func (s *ServerStreamWrapper) SetTrailer(md metadata.MD) {
	s.stream.SetTrailer(md)
}

func (s *ServerStreamWrapper) Context() context.Context {
	return s.context
}

func (s *ServerStreamWrapper) SendMsg(m interface{}) error {
	err := s.stream.SendMsg(m)
	if err != nil && s.span != nil {
		log.Printf(" finishing trace %s", s.span.TraceID())
		s.span.Finish()
	}
	return err
}

func (s *ServerStreamWrapper) RecvMsg(m interface{}) error {
	err := s.stream.RecvMsg(m)
	if err != nil && s.span != nil {
		log.Printf(" finishing trace %s", s.span.TraceID())
		s.span.Finish()
	}
	return err
}

func GRPCStreamServerInterceptor(tc *Client) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		log.Printf("intercepting server")
		if header, ok := md[grpcMetadataKey]; ok {
			span := tc.SpanFromHeader("", strings.Join(header, ""))
			log.Printf(" intercept trace %s", span.TraceID())
			defer func() {
				log.Printf(" defer finishing trace %s", span.TraceID())
				span.Finish()
			}()
			ctx := NewContext(ss.Context(), span)
			ss = &ServerStreamWrapper{stream: ss, span: span, context: ctx}
		}
		return handler(srv, ss)
	}
}
