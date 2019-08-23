// Copyright 2019-present Open Networking Foundation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"context"
	"github.com/atomix/atomix-go-node/pkg/atomix"
	"github.com/atomix/atomix-go-node/pkg/atomix/service"
	"github.com/golang/protobuf/proto"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"os"
	"sync"
	"testing"
	"time"
)

func TestRaftNode(t *testing.T) {
	cluster := atomix.Cluster{
		MemberID: "foo",
		Members: map[string]atomix.Member{
			"foo": {
				ID:   "foo",
				Host: "localhost",
				Port: 5001,
			},
		},
	}

	server := newServer("foo", cluster)
	go server.Start()
	defer server.Stop()
	server.waitForReady()

	client := newRaftClient(ReadConsistency_SEQUENTIAL)
	assert.NoError(t, client.Connect(cluster))

	ch := make(chan service.Output)
	assert.NoError(t, client.Write(context.Background(), newOpenSessionRequest(), ch))
	out := <-ch
	assert.True(t, out.Succeeded())
	openSessionResponse := getOpenSessionResponse(out.Value)
	assert.NotEqual(t, 0, openSessionResponse.SessionID)
	sessionID := openSessionResponse.SessionID

	ch = make(chan service.Output)
	bytes, err := proto.Marshal(&SetRequest{
		Value: "Hello world!",
	})
	assert.NoError(t, err)
	assert.NoError(t, client.Write(context.Background(), newCommandRequest(sessionID, 1, "set", bytes), ch))
	out = <-ch
	assert.True(t, out.Succeeded())
	commandResponse := getCommandResponse(out.Value)
	setResponse := &SetResponse{}
	assert.NoError(t, proto.Unmarshal(commandResponse.Output, setResponse))

	ch = make(chan service.Output)
	bytes, err = proto.Marshal(&GetRequest{})
	assert.NoError(t, err)
	assert.NoError(t, client.Read(context.Background(), newQueryRequest(t, sessionID, commandResponse.Context.Index, 1, "get", bytes), ch))
	out = <-ch
	assert.True(t, out.Succeeded())
	queryResponse := getQueryResponse(out.Value)
	getResponse := &GetResponse{}
	assert.NoError(t, proto.Unmarshal(queryResponse.Output, getResponse))
	assert.Equal(t, "Hello world!", getResponse.Value)
}

func TestRaftCluster(t *testing.T) {
	cluster := atomix.Cluster{
		MemberID: "foo",
		Members: map[string]atomix.Member{
			"foo": {
				ID:   "foo",
				Host: "localhost",
				Port: 5001,
			},
			"bar": {
				ID:   "bar",
				Host: "localhost",
				Port: 5002,
			},
			"baz": {
				ID:   "baz",
				Host: "localhost",
				Port: 5003,
			},
		},
	}

	serverFoo := newServer("foo", cluster)
	serverBar := newServer("bar", cluster)
	serverBaz := newServer("baz", cluster)

	wg := &sync.WaitGroup{}
	wg.Add(3)
	go startServer(serverFoo, wg)
	go startServer(serverBar, wg)
	go startServer(serverBaz, wg)
	wg.Wait()

	defer stopServer(serverFoo)
	defer stopServer(serverBar)
	defer stopServer(serverBaz)
}

func BenchmarkRaftCluster(b *testing.B) {
	log.SetLevel(log.InfoLevel)

	cluster := atomix.Cluster{
		MemberID: "foo",
		Members: map[string]atomix.Member{
			"foo": {
				ID:   "foo",
				Host: "localhost",
				Port: 5001,
			},
			"bar": {
				ID:   "bar",
				Host: "localhost",
				Port: 5002,
			},
			"baz": {
				ID:   "baz",
				Host: "localhost",
				Port: 5003,
			},
		},
	}

	serverFoo := newServer("foo", cluster)
	serverBar := newServer("bar", cluster)
	serverBaz := newServer("baz", cluster)

	wg := &sync.WaitGroup{}
	wg.Add(3)
	go startServer(serverFoo, wg)
	go startServer(serverBar, wg)
	go startServer(serverBaz, wg)
	wg.Wait()

	defer stopServer(serverFoo)
	defer stopServer(serverBar)
	defer stopServer(serverBaz)

	client := newRaftClient(ReadConsistency_SEQUENTIAL)
	assert.NoError(b, client.Connect(cluster))

	ch := make(chan service.Output)
	assert.NoError(b, client.Write(context.Background(), newOpenSessionRequest(), ch))
	out := <-ch
	assert.True(b, out.Succeeded())
	openSessionResponse := getOpenSessionResponse(out.Value)
	assert.NotEqual(b, 0, openSessionResponse.SessionID)
	sessionID := openSessionResponse.SessionID

	b.Run("write", func(b *testing.B) {
		b.ResetTimer()

		ch := make(chan uint64)
		wg := &sync.WaitGroup{}
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				for commandID := range ch {
					ch := make(chan service.Output)
					bytes, _ := proto.Marshal(&SetRequest{
						Value: "Hello world!",
					})
					_ = client.Write(context.Background(), newCommandRequest(sessionID, commandID, "set", bytes), ch)
					out = <-ch
				}
				wg.Done()
			}()
		}

		var commandID uint64
		for n := 0; n < b.N; n++ {
			commandID++
			ch <- commandID
		}
		close(ch)

		wg.Wait()
	})
}

func newServer(memberID string, cluster atomix.Cluster) *RaftServer {
	cluster.MemberID = memberID
	return NewRaftServer(cluster, getServiceRegistry(), 5*time.Second)
}

func startServer(server *RaftServer, wg *sync.WaitGroup) {
	defer wg.Done()
	go func() {
		if err := server.Start(); err != nil {
			wg.Done()
		}
	}()
	_ = server.waitForReady()
}

func newOpenSessionRequest() []byte {
	timeout := 30 * time.Second
	bytes, _ := proto.Marshal(&service.SessionRequest{
		Request: &service.SessionRequest_OpenSession{
			OpenSession: &service.OpenSessionRequest{
				Timeout: &timeout,
			},
		},
	})
	return newTestCommandRequest(bytes)
}

func getOpenSessionResponse(bytes []byte) *service.OpenSessionResponse {
	serviceResponse := &service.ServiceResponse{}
	_ = proto.Unmarshal(bytes, serviceResponse)
	sessionResponse := &service.SessionResponse{}
	_ = proto.Unmarshal(serviceResponse.GetCommand(), sessionResponse)
	return sessionResponse.GetOpenSession()
}

func newKeepAliveRequest(sessionID uint64, commandID uint64, streams map[uint64]uint64) []byte {
	bytes, _ := proto.Marshal(&service.SessionRequest{
		Request: &service.SessionRequest_KeepAlive{
			KeepAlive: &service.KeepAliveRequest{
				SessionID:       sessionID,
				CommandSequence: commandID,
				Streams:         streams,
			},
		},
	})
	return newTestCommandRequest(bytes)
}

func newCloseSessionRequest(sessionID uint64) []byte {
	bytes, _ := proto.Marshal(&service.SessionRequest{
		Request: &service.SessionRequest_CloseSession{
			CloseSession: &service.CloseSessionRequest{
				SessionID: sessionID,
			},
		},
	})
	return newTestCommandRequest(bytes)
}

func newCommandRequest(sessionID uint64, commandID uint64, name string, bytes []byte) []byte {
	bytes, _ = proto.Marshal(&service.SessionRequest{
		Request: &service.SessionRequest_Command{
			Command: &service.SessionCommandRequest{
				Context: &service.SessionCommandContext{
					SessionID:      sessionID,
					SequenceNumber: commandID,
				},
				Name:  name,
				Input: bytes,
			},
		},
	})
	return newTestCommandRequest(bytes)
}

func getCommandResponse(bytes []byte) *service.SessionCommandResponse {
	serviceResponse := &service.ServiceResponse{}
	_ = proto.Unmarshal(bytes, serviceResponse)
	sessionResponse := &service.SessionResponse{}
	_ = proto.Unmarshal(serviceResponse.GetCommand(), sessionResponse)
	return sessionResponse.GetCommand()
}

func newQueryRequest(t *testing.T, sessionID uint64, lastIndex uint64, lastCommandID uint64, name string, bytes []byte) []byte {
	bytes, err := proto.Marshal(&service.SessionRequest{
		Request: &service.SessionRequest_Query{
			Query: &service.SessionQueryRequest{
				Context: &service.SessionQueryContext{
					SessionID:          sessionID,
					LastIndex:          lastIndex,
					LastSequenceNumber: lastCommandID,
				},
				Name:  name,
				Input: bytes,
			},
		},
	})
	assert.NoError(t, err)
	return newTestQueryRequest(t, bytes)
}

func getQueryResponse(bytes []byte) *service.SessionQueryResponse {
	serviceResponse := &service.ServiceResponse{}
	_ = proto.Unmarshal(bytes, serviceResponse)
	sessionResponse := &service.SessionResponse{}
	_ = proto.Unmarshal(serviceResponse.GetQuery(), sessionResponse)
	return sessionResponse.GetQuery()
}

func newTestCommandRequest(bytes []byte) []byte {
	bytes, _ = proto.Marshal(&service.ServiceRequest{
		Id: &service.ServiceId{
			Type:      "test",
			Name:      "test",
			Namespace: "test",
		},
		Request: &service.ServiceRequest_Command{
			Command: bytes,
		},
	})
	return bytes
}

func newTestQueryRequest(t *testing.T, bytes []byte) []byte {
	bytes, err := proto.Marshal(&service.ServiceRequest{
		Id: &service.ServiceId{
			Type:      "test",
			Name:      "test",
			Namespace: "test",
		},
		Request: &service.ServiceRequest_Query{
			Query: bytes,
		},
	})
	assert.NoError(t, err)
	return bytes
}

func getServiceRegistry() *service.ServiceRegistry {
	registry := service.NewServiceRegistry()
	registerTestService(registry)
	return registry
}

func stopServer(server *RaftServer) {
	_ = server.Stop()
}

func init() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.TraceLevel)
}
