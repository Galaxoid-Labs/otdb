package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"sync"

	"github.com/fxamacker/cbor/v2"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/nozzle/throttler"
	"github.com/schollz/progressbar/v3"
)

const ordHost = "http://38.99.82.238:8080"

var (
	retryClient    = retryablehttp.NewClient()
	client         = retryClient.StandardClient()
	inscriptionIDs []string
	inscriptions   = make(map[string]InscriptionExtended)
	batchSize      = 100    // Much higher than this and it actually goes slower. About 7 seconds for 3000 inscriptions, 1.50 seconds for 3000 sats
	block          = 775492 // 767430
)

func main() {
	retryClient.Logger = nil
	retryClient.RetryMax = 10

	fmt.Println("Starting Ord Scraper")
	fmt.Printf("Using Host: %v\n", ordHost)

	highestBlock, err := getHighestBlock()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Current Highest Block: %v\n", highestBlock)

	// get last block written to db and start from there
	// We will always overwrite the last block in the db so we don't need to worry about missing any
	getAndSetStartingBlockFromDB()
	fmt.Printf("Starting at block: %v\n", block)

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
	fmt.Println("")
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
	body, err := makeRequest(fmt.Sprintf("%s/inscription/%v", ordHost, inscriptionID))
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
}

func getAndSetStartingBlockFromDB() int {
	return 0
}

func decodeMetadata(v string) (*string, error) {
	data, err := hex.DecodeString(v)
	if err != nil {
		return nil, err
	}
	opts := cbor.DecOptions{
		MaxArrayElements: 65535,
		MaxMapPairs:      65535,
		MaxNestedLevels:  65535,
		DefaultMapType:   reflect.TypeOf(map[string]interface{}{}),
	}

	d, err := opts.DecMode()
	if err != nil {
		return nil, err
	}

	var metadata interface{}
	err = d.Unmarshal(data, &metadata)
	if err != nil {
		return nil, err
	}

	jsonStr, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	escaped, err := json.Marshal(string(jsonStr))
	if err != nil {
		return nil, err
	}

	strEscaped := string(escaped)

	return &strEscaped, nil
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

type Sat struct {
	Number       uint64   `json:"number"`
	Decimal      string   `json:"decimal"`
	Degree       string   `json:"degree"`
	Name         string   `json:"name"`
	Block        int      `json:"block"`
	Cycle        int      `json:"cycle"`
	Epoch        int      `json:"epoch"`
	Period       int      `json:"period"`
	Offset       int      `json:"offset"`
	Rarity       string   `json:"rarity"`
	Percentile   string   `json:"percentile"`
	Satpoint     string   `json:"satpoint"`
	Timestamp    int      `json:"timestamp"`
	Inscriptions []string `json:"inscriptions"`
}

// type DBInscription struct {
// 	ID        primitive.ObjectID `bson:"_id"`
// 	CreatedAt time.Time          `bson:"created_at"`
// 	UpdatedAt time.Time          `bson:"updated_at"`
// 	Address   string             `bson:"address"`
// 	Children  []string           `bson:"children"`
// 	// ContentLength     int                `bson:"content_length"`
// 	// ContentType       string             `bson:"content_type"`
// 	GenesisFee        int    `bson:"genesis_fee"`
// 	GenesisHeight     uint32 `bson:"genesis_height"`
// 	InscriptionID     string `bson:"inscription_id"`
// 	InscriptionNumber int    `bson:"inscription_number"`
// 	Next              string `bson:"next"`
// 	OutputValue       int    `bson:"output_value"`
// 	Parent            string `bson:"parent,omitempty"`
// 	Previous          string `bson:"previous"`
// 	Rune              string `bson:"rune"` // uint128
// 	Sat               uint64 `bson:"sat,omitempty"`
// 	Satpoint          string `bson:"satpoint,omitempty"`
// }
