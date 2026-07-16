package main

import (
	"log"

	"my_ai_sum/internal/app"
)

func main() {
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
