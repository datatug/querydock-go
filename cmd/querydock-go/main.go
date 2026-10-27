package main

import (
	"log"
	"os"

	"github.com/datatug/querydock-go/internal/command"
	"github.com/datatug/querydock-go/internal/composition"
)

var (
	getenv  = os.Getenv
	compose = composition.Production
	serve   = command.Serve
	report  = func(err error) { log.Print(err) }
	exit    = os.Exit
)

func main() { exit(command.Run(getenv, compose, serve, report)) }
