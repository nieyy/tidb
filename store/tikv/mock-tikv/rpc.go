// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mocktikv

import (
	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
)

type rpcHandler struct {
	cluster   *Cluster
	mvccStore *MvccStore
	storeID   uint64
	startKey  []byte
	endKey    []byte
}

func newRPCHandler(cluster *Cluster, mvccStore *MvccStore, storeID uint64) *rpcHandler {
	return &rpcHandler{
		cluster:   cluster,
		mvccStore: mvccStore,
		storeID:   storeID,
	}
}

func (h *rpcHandler) handleRequest(req *kvrpcpb.Request) *kvrpcpb.Response {
	resp := &kvrpcpb.Response{
		Type: req.Type,
	}
	if err := h.checkContext(req.GetContext()); err != nil {
		resp.RegionError = err
		return resp
	}
	switch req.GetType() {
	case kvrpcpb.MessageType_CmdGet:
		resp.CmdGetResp = h.onGet(req.CmdGetReq)
	case kvrpcpb.MessageType_CmdScan:
		resp.CmdScanResp = h.onScan(req.CmdScanReq)
	case kvrpcpb.MessageType_CmdPrewrite:
		resp.CmdPrewriteResp = h.onPrewrite(req.CmdPrewriteReq)
	case kvrpcpb.MessageType_CmdCommit:
		resp.CmdCommitResp = h.onCommit(req.CmdCommitReq)
	case kvrpcpb.MessageType_CmdCleanup:
		resp.CmdCleanupResp = h.onCleanup(req.CmdCleanupReq)
	case kvrpcpb.MessageType_CmdCommitThenGet:
		resp.CmdCommitGetResp = h.onCommitThenGet(req.CmdCommitGetReq)
	case kvrpcpb.MessageType_CmdRollbackThenGet:
		resp.CmdRbGetResp = h.onRollbackThenGet(req.CmdRbGetReq)
	case kvrpcpb.MessageType_CmdBatchGet:
		resp.CmdBatchGetResp = h.onBatchGet(req.CmdBatchGetReq)
	}
	return resp
}

func (h *rpcHandler) checkContext(ctx *kvrpcpb.Context) *errorpb.Error {
	region, leaderID := h.cluster.GetRegion(ctx.GetRegionId())
	// No region found.
	if region == nil {
		return &errorpb.Error{
			Message: proto.String("region not found"),
			RegionNotFound: &errorpb.RegionNotFound{
				RegionId: proto.Uint64(ctx.GetRegionId()),
			},
		}
	}
	var found bool
	for _, id := range region.StoreIds {
		if id == h.storeID {
			found = true
			break
		}
	}
	// We got the Region, but not in current Store. Also return a NotFound.
	if !found {
		return &errorpb.Error{
			Message: proto.String("region not found"),
			RegionNotFound: &errorpb.RegionNotFound{
				RegionId: proto.Uint64(ctx.GetRegionId()),
			},
		}
	}
	// No leader.
	if leaderID == 0 {
		return &errorpb.Error{
			Message: proto.String("no leader"),
			NotLeader: &errorpb.NotLeader{
				RegionId: proto.Uint64(ctx.GetRegionId()),
			},
		}
	}
	// Leader is not this Store.
	if leaderID != h.storeID {
		return &errorpb.Error{
			Message: proto.String("not leader"),
			NotLeader: &errorpb.NotLeader{
				RegionId:      proto.Uint64(ctx.GetRegionId()),
				LeaderStoreId: proto.Uint64(leaderID),
			},
		}
	}
	// Region epoch does not match.
	if !proto.Equal(region.GetRegionEpoch(), ctx.GetRegionEpoch()) {
		return &errorpb.Error{
			Message:    proto.String("stale epoch"),
			StaleEpoch: &errorpb.StaleEpoch{},
		}
	}
	h.startKey, h.endKey = region.StartKey, region.EndKey
	return nil
}

func (h *rpcHandler) keyInRegion(key []byte) bool {
	return regionContains(h.startKey, h.endKey, key)
}

func (h *rpcHandler) onGet(req *kvrpcpb.CmdGetRequest) *kvrpcpb.CmdGetResponse {
	if !h.keyInRegion(req.Key) {
		panic("onGet: key not in region")
	}

	val, err := h.mvccStore.Get(req.Key, req.GetVersion())
	if err != nil {
		return &kvrpcpb.CmdGetResponse{
			Error: extractKeyError(err),
		}
	}
	return &kvrpcpb.CmdGetResponse{
		Value: val,
	}
}

func (h *rpcHandler) onScan(req *kvrpcpb.CmdScanRequest) *kvrpcpb.CmdScanResponse {
	if !h.keyInRegion(req.GetStartKey()) {
		panic("onScan: startKey not in region")
	}
	pairs := h.mvccStore.Scan(req.GetStartKey(), h.endKey, int(req.GetLimit()), req.GetVersion())
	return &kvrpcpb.CmdScanResponse{
		Pairs: extractPairs(pairs),
	}
}

func (h *rpcHandler) onPrewrite(req *kvrpcpb.CmdPrewriteRequest) *kvrpcpb.CmdPrewriteResponse {
	for _, m := range req.Mutations {
		if !h.keyInRegion(m.Key) {
			panic("onPrewrite: key not in region")
		}
	}
	errors := h.mvccStore.Prewrite(req.Mutations, req.PrimaryLock, req.GetStartVersion())
	return &kvrpcpb.CmdPrewriteResponse{
		Errors: extractKeyErrors(errors),
	}
}

func (h *rpcHandler) onCommit(req *kvrpcpb.CmdCommitRequest) *kvrpcpb.CmdCommitResponse {
	for _, k := range req.Keys {
		if !h.keyInRegion(k) {
			panic("onCommit: key not in region")
		}
	}
	errors := h.mvccStore.Commit(req.Keys, req.GetStartVersion(), req.GetCommitVersion())
	return &kvrpcpb.CmdCommitResponse{
		Errors: []*kvrpcpb.KeyError{extractKeyError(errors)},
	}
}

func (h *rpcHandler) onCleanup(req *kvrpcpb.CmdCleanupRequest) *kvrpcpb.CmdCleanupResponse {
	if !h.keyInRegion(req.Key) {
		panic("onCleanup: key not in region")
	}
	var resp kvrpcpb.CmdCleanupResponse
	err := h.mvccStore.Cleanup(req.Key, req.GetStartVersion())
	if err != nil {
		if commitTS, ok := err.(ErrAlreadyCommitted); ok {
			resp.CommitVersion = proto.Uint64(uint64(commitTS))
		} else {
			resp.Error = extractKeyError(err)
		}
	}
	return &resp
}

func (h *rpcHandler) onCommitThenGet(req *kvrpcpb.CmdCommitThenGetRequest) *kvrpcpb.CmdCommitThenGetResponse {
	if !h.keyInRegion(req.Key) {
		panic("onCommitThenGet: key not in region")
	}
	val, err := h.mvccStore.CommitThenGet(req.Key, req.GetLockVersion(), req.GetCommitVersion(), req.GetGetVersion())
	if err != nil {
		return &kvrpcpb.CmdCommitThenGetResponse{
			Error: extractKeyError(err),
		}
	}
	return &kvrpcpb.CmdCommitThenGetResponse{
		Value: val,
	}
}

func (h *rpcHandler) onRollbackThenGet(req *kvrpcpb.CmdRollbackThenGetRequest) *kvrpcpb.CmdRollbackThenGetResponse {
	if !h.keyInRegion(req.Key) {
		panic("onRollbackThenGet: key not in region")
	}
	val, err := h.mvccStore.RollbackThenGet(req.Key, req.GetLockVersion())
	if err != nil {
		return &kvrpcpb.CmdRollbackThenGetResponse{
			Error: extractKeyError(err),
		}
	}
	return &kvrpcpb.CmdRollbackThenGetResponse{
		Value: val,
	}
}

func (h *rpcHandler) onBatchGet(req *kvrpcpb.CmdBatchGetRequest) *kvrpcpb.CmdBatchGetResponse {
	for _, k := range req.Keys {
		if !h.keyInRegion(k) {
			panic("onBatchGet: key not in region")
		}
	}
	pairs := h.mvccStore.BatchGet(req.Keys, req.GetVersion())
	return &kvrpcpb.CmdBatchGetResponse{
		Pairs: extractPairs(pairs),
	}
}

func extractKeyError(err error) *kvrpcpb.KeyError {
	if locked, ok := err.(*ErrLocked); ok {
		return &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				Key:         locked.Key,
				PrimaryLock: locked.Primary,
				LockVersion: proto.Uint64(locked.StartTS),
			},
		}
	}
	if retryable, ok := err.(ErrRetryable); ok {
		return &kvrpcpb.KeyError{
			Retryable: proto.String(retryable.Error()),
		}
	}
	return &kvrpcpb.KeyError{
		Abort: proto.String(err.Error()),
	}
}

func extractKeyErrors(errs []error) []*kvrpcpb.KeyError {
	var errors []*kvrpcpb.KeyError
	for _, err := range errs {
		errors = append(errors, extractKeyError(err))
	}
	return errors
}

func extractPairs(pairs []Pair) []*kvrpcpb.KvPair {
	var kvPairs []*kvrpcpb.KvPair
	for _, p := range pairs {
		var kvPair *kvrpcpb.KvPair
		if p.Err == nil {
			kvPair = &kvrpcpb.KvPair{
				Key:   p.Key,
				Value: p.Value,
			}
		} else {
			kvPair = &kvrpcpb.KvPair{
				Error: extractKeyError(p.Err),
			}
		}
		kvPairs = append(kvPairs, kvPair)
	}
	return kvPairs
}

// RPCClient sends kv RPC calls to mock cluster.
type RPCClient struct {
	addr      string
	cluster   *Cluster
	mvccStore *MvccStore
}

// SendKVReq sends a kv request to mock cluster.
func (c *RPCClient) SendKVReq(req *kvrpcpb.Request) (*kvrpcpb.Response, error) {
	store := c.cluster.GetStoreByAddr(c.addr)
	if store == nil {
		return nil, errors.New("connect fail")
	}
	handler := newRPCHandler(c.cluster, c.mvccStore, store.GetId())
	return handler.handleRequest(req), nil
}

// SendCopReq sends a coprocessor request to mock cluster.
func (c *RPCClient) SendCopReq(req *coprocessor.Request) (*coprocessor.Response, error) {
	return nil, errors.New("not implemented")
}

// Close closes the client.
func (c *RPCClient) Close() error {
	return nil
}

// NewRPCClient creates an RPCClient.
func NewRPCClient(cluster *Cluster, mvccStore *MvccStore, addr string) *RPCClient {
	return &RPCClient{
		addr:      addr,
		cluster:   cluster,
		mvccStore: mvccStore,
	}
}
