## OTDB is script that will parse ord and write inscriptions to mongodb

### Assumptions
The setup described here assumes running the ord fork below, bitcoin-core with --txindex=1 on the same machine. This is for speed. Mongodb should probably be on a different machine.

The fork of ord adds another inscription endpoint that has more information about an inscription. This is needed for writing to mongodb. It's aslo worth noting that certain things written to the db are probalby not relialbe like genesis_address, etc. Since this data can be updated and it will be quite difficult with this script to keep that synced to db. Extra info thats added to inscriptions are things like: metadata, metaprotocol, charms, recursive refs, etc.

### Requirements
* Beefy Linux server
* Fully indexed ord instance with --index-sats enabled using this fork (https://github.com/Galaxoid-Labs/ord/tree/additional_data_json_response)
* Bitcoin-core with --txindex=1
* MongoDB (running on a different machine is fine) You'll need a sizeable disk for this. Im currently running 56GB Disk, 4GB RAM, 1vCPU though I think more storage and cpu would be better and probably needed for produciton.

### Steps
1. Setup Bitcoin Core with --txindex=1 and fully index
2. Setup ord with --index-sats and fully index using the fork above
    `clone repo, change into dir and run cargo build --release`
    `then copy target/release/ord to ~/bin/ord`
    `then run ord with ~/bin/ord --index-sats server --http --http-port 8080 --csp-origin http://<your_ip> --enable-json-api`
3. Setup mongodb
4. Clone this repo
5. Setup env var ORD_HOST & MONGO_CONNECTION
    `export ORD_HOST='http://127.0.0.1:8080'`
    `export MONGO_CONNECTION='<your mongo connection string>'`
6. Run `go mod tidy` then `go run .`

### Notes
I normally use `screen` to run ord and otdb in the background.
