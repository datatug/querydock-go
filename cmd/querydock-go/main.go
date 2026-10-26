package main

import (
	"log"
	"net/http"
	"os"

	"github.com/datatug/querydock-go/internal/command"
	"github.com/datatug/querydock-go/internal/composition"
)

var (
	getenv  = os.Getenv
	compose = composition.Production
	serve   = (*http.Server).ListenAndServe
	report  = func(err error) { log.Print(err) }
	exit    = os.Exit
)

func main() { exit(command.Run(getenv, compose, serve, report)) }
