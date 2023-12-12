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

const ordHost = "http://38.99.82.238:8080"

var (
	retryClient            = retryablehttp.NewClient()
	client                 = retryClient.StandardClient()
	inscriptionIDs         []string
	inscriptions           = make(map[string]InscriptionExtended)
	batchSize              = 100
	block                  = 767430
	mongoConnection        = os.Getenv("MONGO_CONNECTION")
	mongoClient            *mongo.Client
	mongoCtx               = context.TODO()
	inscriptionsCollection *mongo.Collection
)

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
		Keys:    bson.D{{Key: "inscriptionId", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		panic(err)
	}

	_, err = inscriptionsCollection.Indexes().CreateOne(mongoCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "satRarity", Value: 1}},
		Options: options.Index().SetUnique(false),
	})
	if err != nil {
		panic(err)
	}
	_, err = inscriptionsCollection.Indexes().CreateOne(mongoCtx, mongo.IndexModel{
		Keys:    bson.D{{Key: "contentType", Value: "text"}, {Key: "content", Value: "text"}, {Key: "metaProtocol", Value: "text"}},
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

func main() {
	retryClient.Logger = nil
	retryClient.RetryMax = 10

	fmt.Println("Starting Ord Scraper")
	fmt.Printf("Using Host: %v\n", ordHost)

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
	body, err := makeRequest(fmt.Sprintf("%s/r/blockheight", ordHost))
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
	body, err := makeRequest(fmt.Sprintf("%s/inscriptions/block/%v/%v", ordHost, height, page))
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
	body, err := makeRequest(fmt.Sprintf("%s/e/inscription/%v", ordHost, inscriptionID))
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
		ins = append(ins, DBInscription{
			// Rune:              v.Rune,
			Address:           v.Address,
			Children:          v.Children,
			ContentLength:     v.ContentLength,
			ContentType:       v.ContentType,
			GenesisFee:        v.GenesisFee,
			GenesisHeight:     v.GenesisHeight,
			InscriptionID:     v.InscriptionID,
			InscriptionNumber: strconv.Itoa(v.InscriptionNumber),
			Next:              v.Next,
			OutputValue:       v.OutputValue,
			Parent:            v.Parent,
			Previous:          v.Previous,
			Sat:               strconv.FormatUint(v.Sat, 10),
			Timestamp:         time.Unix(int64(v.Timestamp), 0),
			Satpoint:          v.Satpoint,
			CharmsExtended:    v.CharmsExtended,
			SatRarity:         v.SatRarity,
			MetadataHex:       v.MetadataHex,
			Metadata:          v.Metadata,
			MetaProtocol:      v.MetaProtocol,
			ContentEncoding:   v.ContentEncoding,
			Content:           v.Content,
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
	fmt.Println("")
}

func deleteInscriptionsFromBlock() {
	fmt.Printf("Deleting inscriptions from last written block: %v\n", block)
	_, err := inscriptionsCollection.DeleteMany(mongoCtx, bson.M{"genesisHeight": block})
	if err != nil {
		fmt.Println(err)
	}
}

func getAndSetStartingBlockFromDB() int {
	v, err := inscriptionsCollection.Find(mongoCtx, bson.M{}, options.Find().SetSort(bson.D{{Key: "genesisHeight", Value: -1}}).SetLimit(1))
	if err != nil {
		return block
	}
	var results []DBInscription
	if err = v.All(mongoCtx, &results); err != nil {
		return block
	}
	if len(results) > 0 {
		return int(results[0].GenesisHeight)
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
	GenesisFee        int      `json:"genesis_fee"`
	GenesisHeight     uint32   `json:"genesis_height"`
	InscriptionID     string   `json:"inscription_id"`
	InscriptionNumber int      `json:"inscription_number"`
	Next              string   `json:"next"`
	OutputValue       int      `json:"output_value"`
	Parent            string   `json:"parent,omitempty"`
	Previous          string   `json:"previous"`
	// Rune              interface{} `json:"rune"` // uint128
	Sat             uint64                 `json:"sat,omitempty"`
	Satpoint        string                 `json:"satpoint,omitempty"`
	Timestamp       int                    `json:"timestamp"`
	Charms          uint16                 `json:"charms,omitempty"`          // uint16 representing combination of charms
	CharmsExtended  []Charm                `json:"charms_extended,omitempty"` // Decoded charms with title and icon emoji
	SatRarity       string                 `json:"sat_rarity,omitempty"`
	MetadataHex     string                 `json:"metadata_hex,omitempty"` // CBOR encoded. Decoded on conversion to DB type
	Metadata        map[string]interface{} `json:"metadata,omitempty"`     // CBOR encoded. Decoded on conversion to DB type
	MetaProtocol    string                 `json:"meta_protocol,omitempty"`
	ContentEncoding string                 `json:"content_encoding,omitempty"`
	Content         string                 `json:"content,omitempty"` // Escaped string which could be JSON, Markdown or plain text. Null otherwise
}

type Charm struct {
	Title string `json:"title"`
	Icon  string `json:"icon"`
}

type DBInscription struct {
	// ID primitive.ObjectID `bson:"_id"`
	// CreatedAt time.Time `bson:"created_at"`
	// UpdatedAt time.Time `bson:"updated_at"`
	// Rune              string   `bson:"rune"` // uint128
	Address           string                 `bson:"address"`
	Children          []string               `bson:"children"`
	ContentLength     int                    `bson:"contentLength"`
	ContentType       string                 `bson:"contentType"`
	GenesisFee        int                    `bson:"genesisFee"`
	GenesisHeight     uint32                 `bson:"genesisHeight"`
	InscriptionID     string                 `bson:"inscriptionId"`
	InscriptionNumber string                 `bson:"inscriptionNumber"`
	Next              string                 `bson:"next"`
	OutputValue       int                    `bson:"outputValue"`
	Parent            string                 `bson:"parent,omitempty"`
	Previous          string                 `bson:"previous"`
	Sat               string                 `bson:"sat,omitempty"`
	Satpoint          string                 `bson:"satpoint,omitempty"`
	Timestamp         time.Time              `bson:"timestamp"`
	CharmsExtended    []Charm                `bson:"charmsExtended"` // Decoded charms with title and icon emoji
	SatRarity         string                 `bson:"satRarity"`
	MetadataHex       string                 `bson:"metadataHex"` // CBOR encoded. Decoded on conversion to DB type
	Metadata          map[string]interface{} `bson:"metadata"`    // CBOR decoded json object
	MetaProtocol      string                 `bson:"metaProtocol"`
	ContentEncoding   string                 `bson:"contentEncoding"`
	Content           string                 `bson:"content"` // Escaped string which could be JSON, Markdown or plain text. Null otherwise
}
