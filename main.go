package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	fiber "github.com/chainbound/fiber-go"
	"github.com/chainbound/fiber-go/filter"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

type SniperConfig struct {
	target common.Address
	api    string
	apiKey string
	to     common.Address
}

type BloxrouteSniper struct {
	cfg      SniperConfig
	tag      string
	conn     *websocket.Conn
	sendConn *websocket.Conn
}

func newBloxrouteSniper(cfg SniperConfig) *BloxrouteSniper {
	return &BloxrouteSniper{
		cfg: cfg,
		tag: "bloxroute",
	}
}

func (b *BloxrouteSniper) setupConn() error {
	dialer := websocket.DefaultDialer
	sub, _, err := dialer.Dial(b.cfg.api, http.Header{"Authorization": []string{b.cfg.apiKey}})
	if err != nil {
		return err
	}

	b.sendConn, _, err = dialer.Dial("wss://api.blxrbdn.com/ws", http.Header{"Authorization": []string{b.cfg.apiKey}})
	if err != nil {
		return err
	}

	subReq := fmt.Sprintf(`{"id": 1, "method": "subscribe", "params": ["newTxs", {"include": ["tx_hash", "tx_contents"], "filters": "{from} == '%s'"}]}`, b.cfg.target.Hex())

	err = sub.WriteMessage(websocket.TextMessage, []byte(subReq))
	if err != nil {
		return err
	}

	// Read the first setup message
	if _, _, err := sub.ReadMessage(); err != nil {
		return err
	}

	b.conn = sub
	return nil
}

func (b *BloxrouteSniper) run() error {
	if err := b.setupConn(); err != nil {
		return err
	}

	fmt.Println("Bloxroute connected")

	for {
		var decoded BlxrMsg
		_, msg, err := b.conn.ReadMessage()
		if err != nil {
			log.Println(err)
		}

		ts := time.Now().UnixMilli()

		json.Unmarshal(msg, &decoded)
		hash := decoded.Params.Result.TxHash

		fmt.Println("[Bloxroute]", ts, hash)

		tx, err := b.buildSnipe(decoded.Params.Result.TxContents)
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("Sending transaction...")
		if err := b.sendTransaction(tx); err != nil {
			log.Fatal(err)
		}

		return nil
	}
}

func (b *BloxrouteSniper) buildSnipe(tx BlxrTx) ([]byte, error) {
	snipe := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     uint64(nonce),
		GasTipCap: tx.MaxPriorityFeePerGas.ToInt(),
		GasFeeCap: tx.MaxFeePerGas.ToInt(),
		Gas:       25000,
		To:        &b.cfg.to,
		Value:     big.NewInt(0),
		Data:      []byte(b.tag),
	})

	signed, err := types.SignTx(snipe, signer, privateKey)
	if err != nil {
		return nil, err
	}

	json, _ := signed.MarshalJSON()

	fmt.Println("Built tx", signed.Hash(), string(json))

	return rlp.EncodeToBytes(signed)
}

func (b *BloxrouteSniper) sendTransaction(tx []byte) error {
	hex := hex.EncodeToString(tx)
	fmt.Println("TX hex:", hex)

	msg := fmt.Sprintf(`{"jsonrpc": "2.0", "id": 1, "method": "blxr_tx", "params": {"transaction": "%s"}}`, hex)
	return b.sendConn.WriteMessage(websocket.TextMessage, []byte(msg))
}

type FiberSniper struct {
	cfg SniperConfig
	tag string
}

func newFiberSniper(cfg SniperConfig) *FiberSniper {
	return &FiberSniper{
		cfg: cfg,
		tag: "fiber",
	}
}

func (f *FiberSniper) run() error {
	c := fiber.NewClient(f.cfg.api, f.cfg.apiKey)

	if err := c.Connect(context.Background()); err != nil {
		return err
	}

	defer c.Close()
	fmt.Println("Fiber connected")

	sub := make(chan *fiber.Transaction)

	go c.SubscribeNewTxs(filter.New(filter.From(f.cfg.target.Hex())), sub)

	for tx := range sub {
		ts := time.Now().UnixMilli()
		fmt.Println("[Fiber]\t\t", ts, tx.Hash)

		bytes, err := f.buildSnipe(tx)
		if err != nil {
			return err
		}

		_ = bytes
		hash, ts, err := c.SendRawTransaction(context.Background(), bytes)
		if err != nil {
			return err
		}

		fmt.Println("[Fiber]\t\tTx sent", ts, hash)
		break
	}

	return nil
}

func (b *FiberSniper) buildSnipe(tx *fiber.Transaction) ([]byte, error) {
	snipe := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     uint64(nonce),
		GasTipCap: tx.PriorityFee,
		GasFeeCap: tx.MaxFee,
		Gas:       25000,
		To:        &b.cfg.to,
		Value:     big.NewInt(0),
		Data:      []byte(b.tag),
	})

	signed, err := types.SignTx(snipe, signer, privateKey)
	if err != nil {
		return nil, err
	}

	fmt.Println("Built tx", signed.Hash())

	return signed.MarshalBinary()
}

var privateKey *ecdsa.PrivateKey
var nonce int
var signer types.Signer

func main() {
	var err error

	godotenv.Load()

	blxrKey := os.Getenv("BLXR_API_KEY")
	fiberKey := os.Getenv("FIBER_API_KEY")

	blxrEndpoint := os.Getenv("BLXR_ENDPOINT")
	fiberEndpoint := os.Getenv("FIBER_ENDPOINT")

	senderPk := os.Getenv("SENDER_PK")
	targetAddress := common.HexToAddress(os.Getenv("TARGET_ADDRESS"))

	privateKey, err = crypto.HexToECDSA(senderPk)
	if err != nil {
		log.Fatal(err)
	}

	signer = types.NewLondonSigner(big.NewInt(1))
	to := crypto.PubkeyToAddress(privateKey.PublicKey)

	nonce, err = strconv.Atoi(os.Getenv("NONCE"))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Nonce:", nonce)

	fmt.Println("Bloxroute endpoint:", blxrEndpoint)
	fmt.Println("Fiber endpoint:", fiberEndpoint)

	cfg := SniperConfig{
		target: targetAddress,
		api:    blxrEndpoint,
		apiKey: blxrKey,
		to:     to,
	}

	bloxSniper := newBloxrouteSniper(cfg)

	cfg = SniperConfig{
		target: targetAddress,
		api:    fiberEndpoint,
		apiKey: fiberKey,
		to:     to,
	}

	fiberSniper := newFiberSniper(cfg)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		if err := bloxSniper.run(); err != nil {
			log.Fatal("Running bloxroute:", err)
		}

		wg.Done()
	}()

	go func() {
		if err := fiberSniper.run(); err != nil {
			log.Fatal("Running fiber:", err)
		}

		wg.Done()
	}()

	fmt.Println("Running both snipers...")
	wg.Wait()
}

type BlxrTx struct {
	MaxPriorityFeePerGas *hexutil.Big
	MaxFeePerGas         *hexutil.Big
}

type BlxrMsg struct {
	Params struct {
		Result struct {
			TxHash     common.Hash
			TxContents BlxrTx
		}
	}
}
