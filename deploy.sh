#! bin/bash

env GOOS=linux GOARCH=amd64 go build -o colala .
rsync -avz colala mausved@158.160.86.76:/home/mausved/colala/
rm colala
