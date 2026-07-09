// Package ipcpb holds the generated protobuf types for the EMLyUpdater ⇄
// EMLy named-pipe protocol. Regenerate after editing ../../../proto/updateripc.proto:
//
//	protoc --go_out=. --go_opt=paths=source_relative -I proto proto/updateripc.proto
//
// then move the generated updateripc.pb.go into this directory. protoc and
// protoc-gen-go are not required by CI or `go build`; the generated file is
// committed.
package ipcpb

//go:generate protoc --go_out=. --go_opt=paths=source_relative -I ../../../proto ../../../proto/updateripc.proto
