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

package roles

import (
	"context"
	raft "github.com/atomix/atomix-raft-node/pkg/atomix/raft/protocol"
	"github.com/atomix/atomix-raft-node/pkg/atomix/raft/state"
	"github.com/atomix/atomix-raft-node/pkg/atomix/raft/store"
	"github.com/atomix/atomix-raft-node/pkg/atomix/raft/util"
	"math"
	"math/rand"
	"time"
)

// newFollowerRole returns a new follower role
func newFollowerRole(raft raft.Raft, state state.Manager, store store.Store) raft.Role {
	log := util.NewRoleLogger(string(raft.Member()), string(RoleFollower))
	return &FollowerRole{
		ActiveRole: newActiveRole(raft, state, store, log),
	}
}

// FollowerRole implements a Raft follower
type FollowerRole struct {
	*ActiveRole
	heartbeatTimer *time.Timer
	heartbeatStop  chan bool
}

// Name is the name of the role
func (r *FollowerRole) Name() string {
	return string(RoleFollower)
}

// Start starts the follower
func (r *FollowerRole) Start() error {
	// If there are no other members in the cluster, immediately transition to candidate to increment the term.
	if len(r.raft.Members()) == 1 {
		r.log.Debug("Single node cluster; starting election")
		go r.raft.SetRole(newCandidateRole(r.raft, r.state, r.store))
		return nil
	}
	_ = r.ActiveRole.Start()
	r.resetHeartbeatTimeout()
	return nil
}

// Stop stops the follower
func (r *FollowerRole) Stop() error {
	if r.heartbeatTimer != nil && r.heartbeatTimer.Stop() {
		r.heartbeatStop <- true
	}
	return r.ActiveRole.Stop()
}

// resetHeartbeatTimeout resets the follower's heartbeat timeout
func (r *FollowerRole) resetHeartbeatTimeout() {
	r.raft.WriteLock()
	defer r.raft.WriteUnlock()

	// If a timer is already set, cancel the timer.
	if r.heartbeatTimer != nil {
		if !r.heartbeatTimer.Stop() {
			r.heartbeatStop <- true
			return
		}
	}

	// Set the election timeout in a semi-random fashion with the random range
	// being election timeout and 2 * election timeout.
	timeout := r.raft.Config().GetElectionTimeoutOrDefault() + time.Duration(rand.Int63n(int64(r.raft.Config().GetElectionTimeoutOrDefault())))
	r.heartbeatTimer = time.NewTimer(timeout)
	heartbeatStop := make(chan bool, 1)
	r.heartbeatStop = heartbeatStop
	heartbeatCh := r.heartbeatTimer.C
	go func() {
		select {
		case <-heartbeatCh:
			r.raft.WriteLock()
			if r.active {
				r.raft.SetLeader("")
				r.raft.WriteUnlock()
				r.log.Debug("Heartbeat timed out in %d milliseconds", timeout/time.Millisecond)
				r.sendPollRequests()
			} else {
				r.raft.WriteUnlock()
			}
		case <-heartbeatStop:
			return
		}
	}()
}

// sendPollRequests sends PollRequests to all members of the cluster
func (r *FollowerRole) sendPollRequests() {
	// Set a new timer within which other nodes must respond in order for this node to transition to candidate.
	timeoutTimer := time.NewTimer(r.raft.Config().GetElectionTimeoutOrDefault())
	timeoutExpired := make(chan bool, 1)
	go func() {
		select {
		case <-timeoutTimer.C:
			if r.active {
				r.log.Debug("Failed to poll a majority of the cluster in %d", r.raft.Config().GetElectionTimeoutOrDefault())
				r.resetHeartbeatTimeout()
			}
		case <-timeoutExpired:
			return
		}
	}()

	// Create a quorum that will track the number of nodes that have responded to the poll request.
	votingMembers := r.raft.Members()
	votes := make(chan bool, len(votingMembers))
	quorum := int(math.Floor(float64(len(votingMembers))/2.0) + 1)
	go func() {
		acceptCount := 0
		rejectCount := 0
		for vote := range votes {
			r.raft.ReadLock()
			if !r.active {
				r.raft.ReadUnlock()
				return
			}
			if vote {
				// If no leader has been discovered and the quorum was reached, transition to candidate.
				acceptCount++
				if r.raft.Leader() == "" && acceptCount == quorum {
					r.raft.ReadUnlock()
					r.log.Debug("Received %d/%d pre-votes; transitioning to candidate", acceptCount, len(votingMembers))
					go r.raft.SetRole(newCandidateRole(r.raft, r.state, r.store))
					return
				}
				r.raft.ReadUnlock()
			} else {
				rejectCount++
				if rejectCount == quorum {
					r.log.Debug("Received %d/%d rejected pre-votes; resetting heartbeat timeout", rejectCount, len(votingMembers))
					r.resetHeartbeatTimeout()
					return
				}
				r.raft.ReadUnlock()
			}
		}

		// If not enough votes were received, reset the heartbeat timeout.
		r.resetHeartbeatTimeout()
	}()

	// First, load the last log entry to get its term. We load the entry
	// by its index since the index is required by the protocol.
	r.raft.ReadLock()
	lastEntry := r.store.Writer().LastEntry()
	r.raft.ReadUnlock()
	var lastIndex raft.Index
	if lastEntry != nil {
		lastIndex = lastEntry.Index
	}

	var lastTerm raft.Term
	if lastEntry != nil {
		lastTerm = lastEntry.Entry.Term
	}

	r.log.Debug("Polling members %v", votingMembers)

	// Once we got the last log term, iterate through each current member
	// of the cluster and vote each member for a vote.
	for _, member := range votingMembers {
		// Vote for yourself!
		if member == r.raft.Member() {
			votes <- true
			continue
		}

		go func(member raft.MemberID) {
			r.raft.ReadLock()
			term := r.raft.Term()
			r.raft.ReadUnlock()
			r.log.Debug("Polling %s for next term %d", member, term+1)
			request := &raft.PollRequest{
				Term:         term,
				Candidate:    r.raft.Member(),
				LastLogIndex: lastIndex,
				LastLogTerm:  lastTerm,
			}

			client, err := r.raft.Connect(member)
			if err != nil {
				votes <- false
				r.log.Warn("Poll request failed", err)
			} else {
				r.log.Send("PollRequest", request)
				response, err := client.Poll(context.Background(), request)
				if err != nil {
					votes <- false
					r.log.Warn("Poll request failed", err)
				} else {
					r.log.Receive("PollResponse", response)

					// If the response term is greater than the term we send, use a double checked lock
					// to increment the term.
					if response.Term > term {
						r.raft.WriteLock()
						if response.Term > r.raft.Term() {
							r.raft.SetTerm(response.Term)
						}
						r.raft.WriteUnlock()
					}

					if !response.Accepted {
						r.log.Debug("Received rejected poll from %s", member)
						votes <- false
					} else if response.Term != request.Term {
						r.log.Debug("Received accepted poll for a different term from %s", member)
						votes <- false
					} else {
						r.log.Debug("Received accepted poll from %s", member)
						votes <- true
					}
				}
			}
		}(member)
	}
}

// Configure handles a configure request
func (r *FollowerRole) Configure(ctx context.Context, request *raft.ConfigureRequest) (*raft.ConfigureResponse, error) {
	response, err := r.PassiveRole.Configure(ctx, request)
	r.resetHeartbeatTimeout()
	return response, err
}

// Install handles an install request
func (r *FollowerRole) Install(stream raft.RaftService_InstallServer) error {
	err := r.PassiveRole.Install(stream)
	r.resetHeartbeatTimeout()
	return err
}

// Append handles an append request
func (r *FollowerRole) Append(ctx context.Context, request *raft.AppendRequest) (*raft.AppendResponse, error) {
	response, err := r.PassiveRole.Append(ctx, request)
	r.resetHeartbeatTimeout()
	return response, err
}

// Vote handles a vote request
func (r *FollowerRole) Vote(ctx context.Context, request *raft.VoteRequest) (*raft.VoteResponse, error) {
	r.log.Request("VoteRequest", request)

	// Vote requests can modify the server's vote record, so we need to hold a write lock while handling the request.
	r.raft.WriteLock()

	// If the request indicates a term that is greater than the current term then
	// assign that term and leader to the current context.
	if r.updateTermAndLeader(request.Term, "") {
		go r.raft.SetRole(newFollowerRole(r.raft, r.state, r.store))
	}

	// Handle the vote request and then release the lock
	response, err := r.ActiveRole.handleVote(ctx, request)
	r.raft.WriteUnlock()

	// If we voted for the candidate, reset the heartbeat timeout
	if response.Voted {
		r.resetHeartbeatTimeout()
	}
	_ = r.log.Response("VoteResponse", response, err)
	return response, err
}