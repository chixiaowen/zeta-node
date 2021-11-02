package metaclient

import (
	"context"
	"encoding/hex"
	"github.com/Meta-Protocol/metacore/common"
	"github.com/Meta-Protocol/metacore/metaclient/config"
	"github.com/rs/zerolog/log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Chain configuration struct
// Filled with above constants depending on chain
type ChainObserver struct {
	chain     common.Chain
	router    string
	endpoint  string
	ticker    *time.Ticker
	abiString string
	abi       *abi.ABI
	client    *ethclient.Client
	bridge    *MetachainBridge
	lastBlock uint64
}

// Return configuration based on supplied target chain
func NewChainObserver(chain common.Chain, bridge *MetachainBridge) (*ChainObserver, error) {
	chainOb := ChainObserver{}
	chainOb.bridge = bridge

	// Initialize constants
	switch chain {
	case common.POLYGONChain:
		chainOb.chain = chain
		chainOb.router = config.POLY_ROUTER
		chainOb.endpoint = config.POLY_ENDPOINT
		chainOb.ticker = time.NewTicker(time.Duration(config.POLY_BLOCK_TIME) * time.Second)
		chainOb.abiString = config.META_ABI
	case common.ETHChain:
		chainOb.chain = chain
		chainOb.router = config.ETH_ROUTER
		chainOb.endpoint = config.ETH_ENDPOINT
		chainOb.ticker = time.NewTicker(time.Duration(config.ETH_BLOCK_TIME) * time.Second)
		chainOb.abiString = config.META_LOCK_ABI
	case common.BSCChain:
		chainOb.chain = chain
		chainOb.router = config.BSC_ROUTER
		chainOb.endpoint = config.BSC_ENDPOINT
		chainOb.ticker = time.NewTicker(time.Duration(config.BSC_BLOCK_TIME) * time.Second)
		chainOb.abiString = config.BSC_META_ABI
	}
	contractABI, err := abi.JSON(strings.NewReader(chainOb.abiString))
	if err != nil {
		return nil, err
	}
	chainOb.abi = &contractABI

	// Dial the router
	client, err := ethclient.Dial(chainOb.endpoint)
	if err != nil {
		log.Err(err).Msg("eth client Dial")
		return nil, err
	}
	chainOb.client = client
	chainOb.lastBlock = chainOb.setLastBlock()
	// if ZetaCore does not have last heard block height, then use current
	if chainOb.lastBlock == 0 {
		header, err := chainOb.client.HeaderByNumber(context.Background(), nil)
		if err != nil {
			return nil, err
		}
		chainOb.lastBlock = header.Number.Uint64()
	}

	return &chainOb, nil
}

func (chainOb *ChainObserver) WatchRouter() {
	// At each tick, query the router
	for range chainOb.ticker.C {
		err := chainOb.queryRouter()
		if err != nil {
			log.Err(err).Msg("queryRouter error")
			continue
		}
	}
}

func (chainOb *ChainObserver) queryRouter() error {
	header, err := chainOb.client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return err
	}
	// skip if no new block is produced.
	if header.Number.Uint64() <= chainOb.lastBlock {
		return nil
	}
	toBlock := chainOb.lastBlock + 10 // read 10 blocks at time at most
	if toBlock >= header.Number.Uint64() {
		toBlock = header.Number.Uint64()
	}
	query := ethereum.FilterQuery{
		Addresses: []ethcommon.Address{ethcommon.HexToAddress(chainOb.router)},
		FromBlock: big.NewInt(0).SetUint64(chainOb.lastBlock + 1), // lastBlock has been processed;
		ToBlock:   big.NewInt(0).SetUint64(toBlock),
	}
	log.Debug().Msgf("signer %s block from %d to %d", chainOb.bridge.GetKeys().signerName, query.FromBlock, query.ToBlock)

	// Finally query the for the logs
	logs, err := chainOb.client.FilterLogs(context.Background(), query)
	if err != nil {
		return err
	}

	// Read in ABI
	contractAbi := chainOb.abi

	// LockSend event signature
	logLockSendSignature := []byte("LockSend(address,string,uint256,string,bytes)")
	logLockSendSignatureHash := crypto.Keccak256Hash(logLockSendSignature)

	// Unlock event signature
	logUnlockSignature := []byte("Unlock(address,uint256)")
	logUnlockSignatureHash := crypto.Keccak256Hash(logUnlockSignature)

	// BurnSend event signature
	logBurnSendSignature := []byte("BurnSend(address,address,uint256,uint256,string)")
	logBurnSendSignatureHash := crypto.Keccak256Hash(logBurnSendSignature)

	// MMinted event signature
	logMMintedSignature := []byte("MMinted(address,uint256,bytes32)")
	logMMintedSignatureHash := crypto.Keccak256Hash(logMMintedSignature)

	// Pull out arguments from logs
	for _, vLog := range logs {
		log.Debug().Msgf("TxBlockNumber %d Transaction Hash: %s topic %s\n", vLog.BlockNumber, vLog.TxHash.Hex()[:6], vLog.Topics[0].Hex()[:6])

		switch vLog.Topics[0].Hex() {
		case logLockSendSignatureHash.Hex():
			returnVal, err := contractAbi.Unpack("LockSend", vLog.Data)
			if err != nil {
				log.Err(err).Msg("error unpacking LockSend")
				continue
			}

			// PostSend to meta core
			metaHash, err := chainOb.bridge.PostSend(
				returnVal[0].(ethcommon.Address).String(),
				chainOb.chain.String(),
				returnVal[1].(string),
				returnVal[3].(string),
				returnVal[2].(*big.Int).String(),
				"0",
				string(returnVal[4].([]uint8)), // TODO: figure out appropriate format for message
				vLog.TxHash.Hex(),
				vLog.BlockNumber,
			)
			if err != nil {
				log.Err(err).Msg("error posting to meta core")
				continue
			}
			log.Debug().Msgf("LockSend detected: PostSend metahash: %s", metaHash)
		case logBurnSendSignatureHash.Hex():
			returnVal, err := contractAbi.Unpack("BurnSend", vLog.Data)
			if err != nil {
				log.Err(err).Msg("error unpacking LockSend")
				continue
			}

			// PostSend to meta core
			metaHash, err := chainOb.bridge.PostSend(
				returnVal[0].(ethcommon.Address).String(),
				chainOb.chain.String(),
				returnVal[1].(ethcommon.Address).String(),
				returnVal[3].(*big.Int).String(),
				returnVal[2].(*big.Int).String(),
				"0",
				returnVal[4].(string), // TODO: figure out appropriate format for message
				vLog.TxHash.Hex(),
				vLog.BlockNumber,
			)
			if err != nil {
				log.Err(err).Msg("error posting to meta core")
				continue
			}

			log.Debug().Msgf("BurnSend detected: PostSend metahash: %s", metaHash)
		case logUnlockSignatureHash.Hex():
			returnVal, err := contractAbi.Unpack("Unlock", vLog.Data)
			if err != nil {
				log.Err(err).Msg("error unpacking Unlock")
				continue
			}

			// Post confirmation to meta core
			var sendHash, outTxHash string

			// sendHash = empty string for now
			// outTxHash = tx hash returned by signer.MMint
			var rxAddress string = returnVal[0].(ethcommon.Address).String()
			var mMint string = returnVal[1].(*big.Int).String()
			metaHash, err := chainOb.bridge.PostReceiveConfirmation(
				sendHash,
				outTxHash,
				vLog.BlockNumber,
				mMint,
			)
			if err != nil {
				log.Err(err).Msg("error posting confirmation to meta score")
				continue
			}
			log.Debug().Msgf("Unlock detected; recv %s Post confirmation meta hash %s", rxAddress, metaHash[:6])

		case logMMintedSignatureHash.Hex():
			returnVal, err := contractAbi.Unpack("MMinted", vLog.Data)
			if err != nil {
				log.Err(err).Msg("error unpacking Unlock")
				continue
			}

			// outTxHash = tx hash returned by signer.MMint
			rxAddress := returnVal[0].(ethcommon.Address).String()
			mMint := returnVal[1].(*big.Int).String()
			sendhash := returnVal[2].([32]byte)
			sendHash := "0x" + hex.EncodeToString(sendhash[:])
			metaHash, err := chainOb.bridge.PostReceiveConfirmation(
				sendHash,
				vLog.TxHash.Hex(),
				vLog.BlockNumber,
				mMint,
			)
			if err != nil {
				log.Err(err).Msg("error posting confirmation to meta score")
				continue
			}
			log.Debug().Msgf("MMinted event detected; recv %s Post confirmation meta hash %s", rxAddress, metaHash[:6])
			log.Debug().Msgf("MMinted(sendhash=%s, outTxHash=%s, blockHeight=%d, mMint=%s", sendHash[:6], vLog.TxHash.Hex()[:6], vLog.BlockNumber, mMint)
		}
	}

	chainOb.lastBlock = toBlock

	return nil
}

// query ZetaCore about the last block that it has heard from a specific chain.
// return 0 if not existent.
func (chainOb *ChainObserver) setLastBlock() uint64 {
	lastheight, err := chainOb.bridge.GetLastBlockHeightByChain(chainOb.chain)
	if err != nil {
		log.Warn().Err(err).Msgf("setLastBlock")
		return 0
	}
	return lastheight.LastSendHeight
}
