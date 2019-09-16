package main

import (
	"flag"
	"fmt"

	"github.com/rumblefrog/discordgo"
)

// Variables used for command line parameters
var (
	Token string
)

func init() {

	flag.StringVar(&Token, "t", "", "Account Token")
	flag.Parse()
}

func main() {

	// Create a new Discord session using the provided login information.
	dg, err := discordgo.New(Token)
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}

	resp, err := dg.GatewayBot()
	fmt.Printf("%#v\n\n%v\n", resp, err)
}
