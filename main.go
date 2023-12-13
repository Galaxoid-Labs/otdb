package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/nozzle/throttler"
	"github.com/schollz/progressbar/v3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// const ordHost = "http://38.99.82.238:8080"

var (
	retryClient            = retryablehttp.NewClient()
	client                 = retryClient.StandardClient()
	inscriptionIDs         []string
	inscriptions           = make(map[string]InscriptionExtended)
	batchSize              = 500
	block                  = 767430
	host                   = os.Getenv("ORD_HOST")
	mongoConnection        = os.Getenv("MONGO_CONNECTION")
	mongoClient            *mongo.Client
	mongoCtx               = context.TODO()
	inscriptionsCollection *mongo.Collection
)

func main() {
	retryClient.Logger = nil
	retryClient.RetryMax = 10

	if host == "" {
		panic("ORD_HOST env var not set")
	}

	if mongoConnection == "" {
		panic("MONGO_CONNECTION env var not set")
	}

	fmt.Println("Starting Ord Scraper")
	fmt.Printf("Using Host: %v\n", host)

	ConnectMongoDB()

	highestBlock, err := getHighestBlock()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Current Highest Block: %v\n", highestBlock)

	// get last block written to db and start from there
	// We will always overwrite the last block in the db so we don't need to worry about missing any
	lastBlockWritten := getAndSetStartingBlockFromDB()
	if lastBlockWritten != block {
		block = lastBlockWritten
		deleteInscriptionsFromBlock()
	}
	fmt.Printf("Starting at block: %v\n\n", block)

	for block <= highestBlock {

		getAndWriteInscriptionIDs()
		getAndWriteInscriptions()
		convertAndWriteInscriptionsToDB()

		// Reset for next block
		inscriptionIDs = []string{}
		inscriptions = make(map[string]InscriptionExtended)

		block++
	}

	fmt.Printf("Total Inscriptions: %v\n", len(inscriptions))
}

func ConnectMongoDB() {
	fmt.Println("Connecting to MongoDB")
	mongoOptions := options.Client().ApplyURI(mongoConnection)

	var err error
	mongoClient, err = mongo.Connect(mongoCtx, mongoOptions)
	if err != nil {
		panic(err)
	}

	err = mongoClient.Ping(mongoCtx, nil)
	if err != nil {
		panic(err)
	}

	inscriptionsCollection = mongoClient.Database("ord").Collection("inscriptions")
	if inscriptionsCollection == nil {
		panic("inscriptionsCollection is nil")
	}
	_, err = inscriptionsCollection.Indexes().CreateOne(mongoCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		panic(err)
	}

	_, err = inscriptionsCollection.Indexes().CreateOne(mongoCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "sat_rarity", Value: 1}},
		Options: options.Index().SetUnique(false),
	})
	if err != nil {
		panic(err)
	}

	_, err = inscriptionsCollection.Indexes().CreateOne(mongoCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "content_type", Value: "text"}, {Key: "content", Value: "text"}, {Key: "metaprotocol", Value: "text"}},
		Options: options.Index().SetUnique(false),
	})
	if err != nil {
		panic(err)
	}
	_, err = inscriptionsCollection.Indexes().CreateOne(mongoCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "metadata.$**", Value: 1}},
		Options: options.Index().SetUnique(false),
	})
	if err != nil {
		panic(err)
	}
}

func getAndWriteInscriptionIDs() {
	spinner := progressbar.Default(-1, fmt.Sprintf("Fetching Inscription ID's for Block: %v", block))
	more := true
	page := 0
	for more {
		b, err := getBlock(block, page)
		if err != nil {
			fmt.Println(err)
		}
		inscriptionIDs = append(inscriptionIDs, b.Inscriptions...)
		page++
		more = b.More
		spinner.Add(1)
	}
	spinner.Finish()
	fmt.Print("\033[F")
}

func getAndWriteInscriptions() {
	mutex := &sync.RWMutex{}
	bar := progressbar.Default(int64(len(inscriptionIDs)), fmt.Sprintf("Fetching Inscriptions for Block: %v", block))
	t := throttler.New(batchSize, len(inscriptionIDs))
	for _, id := range inscriptionIDs {
		go func(id string) {
			ins, err := getInscription(id)
			if err != nil {
				// TODO: Maybe retry this block on error.
				panic(err)
			}
			mutex.Lock()
			inscriptions[ins.InscriptionID] = *ins
			mutex.Unlock()
			bar.Add(1)
			t.Done(nil)
		}(id)
		t.Throttle()
	}
}

func getHighestBlock() (int, error) {
	body, err := makeRequest(fmt.Sprintf("%s/r/blockheight", host))
	if err != nil {
		return -1, err
	}

	height, err := strconv.Atoi(string(body))
	if err != nil {
		return -1, err
	}

	return height, nil
}

func getBlock(height int, page int) (*Block, error) {
	body, err := makeRequest(fmt.Sprintf("%s/inscriptions/block/%v/%v", host, height, page))
	if err != nil {
		return nil, err
	}

	var result Block
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func getInscription(inscriptionID string) (*InscriptionExtended, error) {
	body, err := makeRequest(fmt.Sprintf("%s/e/inscription/%v", host, inscriptionID))
	if err != nil {
		return nil, err
	}

	var result InscriptionExtended
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func makeRequest(url string) ([]byte, error) {
	request, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func convertAndWriteInscriptionsToDB() {
	if len(inscriptions) == 0 {
		fmt.Printf("No Inscriptions to write to DB for block: %v\n", block)
		fmt.Println("")
		return
	}
	ins := []DBInscription{}
	for _, v := range inscriptions {
		date := time.Now()
		ins = append(ins, DBInscription{
			CreatedAt:          date,
			UpdatedAt:          date,
			ID:                 v.InscriptionID,
			Number:             v.InscriptionNumber,
			Address:            v.Address,
			GenesisAddress:     v.Address,
			GenesisBlockHeight: v.GenesisHeight,
			GenesisBlockHash:   v.BlockHash,
			GenesisTxID:        v.TxID,
			GenesisTimestamp:   v.Timestamp,
			TxID:               v.TxID,
			Location:           v.Satpoint,
			Output:             v.SatpointOutpoint,
			Value:              strconv.FormatUint(v.OutputValue, 10),
			SatOrdinal:         strconv.FormatUint(v.Sat, 10),
			SatRarity:          v.SatRarity,
			ContentType:        v.ContentType,
			ContentLength:      v.ContentLength,
			Timestamp:          time.Unix(int64(v.Timestamp), 0),
			Offset:             strconv.FormatUint(v.SatpointOffset, 10),

			// Rune:              v.Rune,

			Children:        v.Children,
			Charms:          v.Charms,
			CharmsExtended:  v.CharmsExtended,
			MetadataHex:     v.MetadataHex,
			Metadata:        v.Metadata,
			MetaProtocol:    v.MetaProtocol,
			ContentEncoding: v.ContentEncoding,
			Content:         v.Content,
			Recursive:       v.Recursive,
			RecursiveRefs:   v.RecursiveRefs,
			Next:            v.Next,
			OutputValue:     strconv.FormatUint(v.OutputValue, 10),
			Parent:          v.Parent,
			Previous:        v.Previous,
			Sat:             strconv.FormatUint(v.Sat, 10),
			Satpoint:        v.Satpoint,
		})
	}

	var coll []interface{}
	for _, s := range ins {
		coll = append(coll, s)
	}

	_, err := inscriptionsCollection.InsertMany(mongoCtx, coll, options.InsertMany().SetOrdered(false))
	if err != nil {
		fmt.Println(err)
	}

	fmt.Printf("Wrote %v inscriptions to DB for block: %v\n", len(inscriptions), block)
	for _, v := range inscriptions {
		fmt.Printf("Block Timestamp: %v\n", time.Unix(int64(v.Timestamp), 0).Format(time.RFC1123))
		break
	}
	fmt.Println("")
}

func deleteInscriptionsFromBlock() {
	fmt.Printf("Deleting inscriptions from last written block: %v\n", block)
	_, err := inscriptionsCollection.DeleteMany(mongoCtx, bson.M{"genesis_block_height": block})
	if err != nil {
		fmt.Println(err)
	}
}

func getAndSetStartingBlockFromDB() int {
	v, err := inscriptionsCollection.Find(mongoCtx, bson.M{}, options.Find().SetSort(bson.D{{Key: "genesis_block_height", Value: -1}}).SetLimit(1))
	if err != nil {
		return block
	}
	var results []DBInscription
	if err = v.All(mongoCtx, &results); err != nil {
		return block
	}
	if len(results) > 0 {
		return int(results[0].GenesisBlockHeight)
	}
	return block
}

func decodeMetadata(v string) string {
	data, err := hex.DecodeString(v)
	if err != nil {
		return ""
	}
	opts := cbor.DecOptions{
		MaxArrayElements: 65535,
		MaxMapPairs:      65535,
		MaxNestedLevels:  65535,
		DefaultMapType:   reflect.TypeOf(map[string]interface{}{}),
	}

	d, err := opts.DecMode()
	if err != nil {
		return ""
	}

	var metadata interface{}
	err = d.Unmarshal(data, &metadata)
	if err != nil {
		return ""
	}

	jsonStr, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}

	escaped, err := json.Marshal(string(jsonStr))
	if err != nil {
		return ""
	}

	strEscaped := string(escaped)

	return strEscaped
}

// func formatJSON(data []byte) string {
// 	var out bytes.Buffer
// 	err := json.Indent(&out, data, "", " ")
// 	if err != nil {
// 		fmt.Println(err)
// 	}

// 	d := out.Bytes()
// 	return string(d)
// }

type Block struct {
	Inscriptions []string `json:"inscriptions"`
	More         bool     `json:"more"`
	PageIndex    int      `json:"page_index"`
}

type InscriptionExtended struct {
	Address           string   `json:"address"`
	Children          []string `json:"children"`
	ContentLength     int      `json:"content_length"`
	ContentType       string   `json:"content_type"`
	GenesisFee        uint64   `json:"genesis_fee"`
	GenesisHeight     uint32   `json:"genesis_height"`
	InscriptionID     string   `json:"inscription_id"`
	InscriptionNumber int32    `json:"inscription_number"`
	Next              string   `json:"next"`
	OutputValue       uint64   `json:"output_value"`
	Parent            string   `json:"parent,omitempty"`
	Previous          string   `json:"previous"`
	// Rune              interface{} `json:"rune"` // uint128
	Sat              uint64                 `json:"sat,omitempty"`
	Satpoint         string                 `json:"satpoint,omitempty"`
	Timestamp        int64                  `json:"timestamp"`
	Charms           uint16                 `json:"charms,omitempty"`          // uint16 representing combination of charms
	CharmsExtended   []Charm                `json:"charms_extended,omitempty"` // Decoded charms with title and icon emoji
	SatRarity        string                 `json:"sat_rarity,omitempty"`
	MetadataHex      string                 `json:"metadata_hex,omitempty"` // CBOR encoded. Decoded on conversion to DB type
	Metadata         map[string]interface{} `json:"metadata,omitempty"`     // CBOR encoded. Decoded on conversion to DB type
	MetaProtocol     string                 `json:"meta_protocol,omitempty"`
	ContentEncoding  string                 `json:"content_encoding,omitempty"`
	Content          string                 `json:"content,omitempty"` // Escaped string which could be JSON, Markdown or plain text. Null otherwise
	Recursive        bool                   `json:"recursive,omitempty"`
	RecursiveRefs    []string               `json:"recursive_refs,omitempty"`
	TxID             string                 `json:"tx_id,omitempty"`
	BlockHash        string                 `json:"block_hash,omitempty"`
	SatpointOutpoint string                 `json:"satpoint_outpoint,omitempty"`
	SatpointOffset   uint64                 `json:"satpoint_offset,omitempty"`
}

type Charm struct {
	Title string `json:"title"`
	Icon  string `json:"icon"`
}

type DBInscription struct {
	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
	// Rune              string   `bson:"rune"` // uint128
	ID                 string `bson:"id"`
	Number             int32  `bson:"number"`
	Address            string `bson:"address"`
	GenesisAddress     string `bson:"genesis_address"`
	GenesisBlockHeight uint32 `bson:"genesis_block_height"`
	GenesisBlockHash   string `bson:"genesis_block_hash"`
	GenesisTxID        string `bson:"genesis_tx_id"`
	GenesisTimestamp   int64  `bson:"genesis_timestamp"`
	TxID               string `bson:"tx_id"`
	Location           string `bson:"location"`
	Output             string `bson:"output"`
	Value              string `bson:"value"`
	Offset             string `bson:"offset"`
	SatOrdinal         string `bson:"sat_ordinal"`
	SatRarity          string `bson:"sat_rarity"`
	// SatCoinbaseHeight string                 `bson:"sat_coinbase_height"`
	ContentType     string                 `bson:"content_type"`
	ContentLength   int                    `bson:"contentLength"`
	Timestamp       time.Time              `bson:"timestamp"`
	Recursive       bool                   `bson:"recursive,omitempty"`
	RecursiveRefs   []string               `bson:"recursive_refs,omitempty"`
	Children        []string               `bson:"children"`
	Charms          uint16                 `bson:"charms,omitempty"` // uint16 representing combination of charms
	CharmsExtended  []Charm                `bson:"charms_extended"`  // Decoded charms with title and icon emoji
	MetadataHex     string                 `bson:"metadata_hex"`     // CBOR encoded. Decoded on conversion to DB type
	Metadata        map[string]interface{} `bson:"metadata"`         // CBOR decoded json object
	MetaProtocol    string                 `bson:"metaprotocol"`
	ContentEncoding string                 `bson:"content_encoding"`
	Content         string                 `bson:"content"` // Escaped string which could be JSON, Markdown or plain text. Null otherwise
	Next            string                 `bson:"next"`
	OutputValue     string                 `bson:"outputValue"`
	Parent          string                 `bson:"parent,omitempty"`
	Previous        string                 `bson:"previous"`
	Sat             string                 `bson:"sat,omitempty"`
	Satpoint        string                 `bson:"satpoint,omitempty"`
}
