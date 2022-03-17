// (c) 2021-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package statesyncclient

import (
	"context"

	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/plugin/evm/message"
	"github.com/ava-labs/coreth/sync/handlers"
	"github.com/ethereum/go-ethereum/common"
)

var _ Client = &MockClient{}

// TODO replace with gomock library
type MockClient struct {
	codec          codec.Manager
	leafsHandler   *handlers.LeafsRequestHandler
	LeavesReceived int
	codesHandler   *handlers.CodeRequestHandler
	CodeReceived   int
	blocksHandler  *handlers.BlockRequestHandler
	BlocksReceived int
	// GetLeafsIntercept is called on every GetLeafs request if set to a non-nil callback.
	// Takes in the result returned by the handler and can return a replacement response or
	// error.
	GetLeafsIntercept func(message.LeafsResponse) (message.LeafsResponse, error)
	// GetCodesIntercept is called on every GetCode request if set to a non-nil callback.
	// Takes in the result returned by the handler and can return a replacement response or
	// error.
	GetCodeIntercept func([]byte) ([]byte, error)
	// GetBlocksIntercept is called on every GetBlocks request if set to a non-nil callback.
	// Takes in the result returned by the handler and can return a replacement response or
	// error.
	GetBlocksIntercept func(types.Blocks) (types.Blocks, error)
}

func NewMockClient(
	codec codec.Manager,
	leafHandler *handlers.LeafsRequestHandler,
	codesHandler *handlers.CodeRequestHandler,
	blocksHandler *handlers.BlockRequestHandler,
) *MockClient {
	return &MockClient{
		codec:         codec,
		leafsHandler:  leafHandler,
		codesHandler:  codesHandler,
		blocksHandler: blocksHandler,
	}
}

func (ml *MockClient) GetLeafs(request message.LeafsRequest) (message.LeafsResponse, error) {
	response, err := ml.leafsHandler.OnLeafsRequest(context.Background(), ids.GenerateTestShortID(), 1, request)
	if err != nil {
		return message.LeafsResponse{}, err
	}

	leafResponseIntf, numLeaves, err := parseLeafsResponse(ml.codec, request, response)
	if err != nil {
		return message.LeafsResponse{}, err
	}
	leafsResponse := leafResponseIntf.(message.LeafsResponse)
	if ml.GetLeafsIntercept != nil {
		leafsResponse, err = ml.GetLeafsIntercept(leafsResponse)
	}
	// Increment the number of leaves received by the mock client
	ml.LeavesReceived += numLeaves
	return leafsResponse, err
}

func (ml *MockClient) GetCode(codeHash common.Hash) ([]byte, error) {
	if ml.codesHandler == nil {
		panic("no code handler for mock client")
	}
	request := message.CodeRequest{Hash: codeHash}
	response, err := ml.codesHandler.OnCodeRequest(context.Background(), ids.GenerateTestShortID(), 1, request)
	if err != nil {
		return nil, err
	}

	codeBytesIntf, lenCode, err := parseCode(ml.codec, request, response)
	if err != nil {
		return nil, err
	}
	code := codeBytesIntf.([]byte)
	if ml.GetCodeIntercept != nil {
		code, err = ml.GetCodeIntercept(code)
	}
	if err == nil {
		ml.CodeReceived += lenCode
	}
	return code, err
}

func (ml *MockClient) GetBlocks(blockHash common.Hash, height uint64, numParents uint16) ([]*types.Block, error) {
	if ml.blocksHandler == nil {
		panic("no blocks handler for mock client")
	}
	request := message.BlockRequest{
		Hash:    blockHash,
		Height:  height,
		Parents: numParents,
	}
	response, err := ml.blocksHandler.OnBlockRequest(context.Background(), ids.GenerateTestShortID(), 1, request)
	if err != nil {
		return nil, err
	}

	blocksRes, numBlocks, err := parseBlocks(ml.codec, request, response)
	if err != nil {
		return nil, err
	}
	blocks := blocksRes.(types.Blocks)
	if ml.GetBlocksIntercept != nil {
		blocks, err = ml.GetBlocksIntercept(blocks)
	}
	ml.BlocksReceived += numBlocks
	return blocks, err
}