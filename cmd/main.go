package main

import (
	"context"
	"github.com/jirevwe/xplug"
	"log"
)

func main() {
	//xplug.Main()
	err := xplug.Build(context.Background(), "--with", "github.com/jirevwe/http-plugin@v1.0.0")
	if err != nil {
		log.Fatal(err)
	}
}
