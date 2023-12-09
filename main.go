package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/nozzle/throttler"
	"github.com/schollz/progressbar/v3"
)

const ordHost = "http://38.99.82.238:8080"

var (
	client         = &http.Client{}
	inscriptionIDs []string
	inscriptions   = make(map[string]Inscription)
	sats           = make(map[string]Sat)
	batchSize      = 200 // Much higher than this and it actually goes slower. About 7 seconds for 3000 inscriptions, 1.50 seconds for 3000 sats
)

func main() {
	// get block
	// // get all pages using more, page_index
	// // get each inscription
	// // // get metadata for each inscription and add to full object
	// /// //
	// once block is finished batch write all full objects to db
	// repeat for next block

	fmt.Println("Starting Ord Scraper")
	fmt.Printf("Using Host: %v\n", ordHost)

	// highestBlock, err := getHighestBlock()
	// if err != nil {
	// 	panic(err)
	// }
	highestBlock := 800000 // TODO: remove this
	fmt.Printf("Current Highest Block: %v\n", highestBlock)

	// TODO: get last block written to db and start from there
	// We will always overwrite the last block in the db so we don't need to worry about missing any
	block := 800000
	fmt.Printf("Starting at block: %v\n", block)

	for block <= highestBlock {

		// Get all inscriptions for block
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

		fmt.Printf("Block: %v, Inscriptions: %v\n", block, len(inscriptionIDs))

		mutex := &sync.RWMutex{}
		bar := progressbar.Default(int64(len(inscriptionIDs)), fmt.Sprintf("Fetching Inscriptions for Block: %v", block))
		t := throttler.New(batchSize, len(inscriptionIDs))
		for _, id := range inscriptionIDs {
			go func(id string) {
				ins, err := getInscription(id)
				if err != nil {
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

		// Fetch sat data and update inscriptions
		bar = progressbar.Default(int64(len(inscriptionIDs)), "Fetching Sat Data for Inscriptions")
		t = throttler.New(batchSize, len(inscriptionIDs))
		for k, ins := range inscriptions {
			go func(sat uint64, id string) {
				s, err := getSat(sat)
				if err != nil {
					panic(err)
				}
				mutex.Lock()
				sats[id] = *s
				mutex.Unlock()
				bar.Add(1)
				t.Done(nil)
			}(ins.Sat, k)
			t.Throttle()
		}

		// TODO: write to db

		block++
	}

	fmt.Printf("Total Inscriptions: %v\n", len(inscriptions))
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

func getInscription(inscriptionID string) (*Inscription, error) {
	body, err := makeRequest(fmt.Sprintf("%s/inscription/%v", ordHost, inscriptionID))
	if err != nil {
		return nil, err
	}

	var result Inscription
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func getSat(sat uint64) (*Sat, error) {
	body, err := makeRequest(fmt.Sprintf("%s/sat/%v", ordHost, sat))
	if err != nil {
		return nil, err
	}

	var result Sat
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

func removeDuplicate(strSlice []string) []string {
	allKeys := make(map[string]bool)
	list := []string{}
	for _, item := range strSlice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
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

type Inscription struct {
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
	Sat       uint64 `json:"sat,omitempty"`
	Satpoint  string `json:"satpoint,omitempty"`
	Timestamp int    `json:"timestamp"`
	// SatData   *Sat
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
