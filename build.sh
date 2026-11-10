#! /bin/bash

go build -o bin/server ./cmd/server
go build -o bin/client ./cmd/client
go build -o bin/dashboard ./cmd/dashboard 