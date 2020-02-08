package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jonas747/discordgo"
)

// Variables used for command line parameters
var (
	Token string
	vc    *discordgo.VoiceConnection

	sID int
)

func init() {

	flag.StringVar(&Token, "t", "", "Bot Token")
	flag.IntVar(&sID, "sid", 0, "Shard ID")

	flag.Parse()
}

func main() {
	fmt.Println("Using ", sID)

	// Create a new Discord session using the provided bot token.
	dg, err := discordgo.New("Bot " + Token)
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}

	dg.ShardCount = 192
	dg.ShardID = sID

	dg.LogLevel = discordgo.LogDebug

	// manager := dshardmanager.New("Bot " + Token)
	// manager.SessionFunc = func(token string) (*discordgo.Session, error) {
	// 	session, err := discordgo.New(token)
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	session.LogLevel = discordgo.LogDebug
	// 	return session, nil
	// }
	// err := manager.Start()
	// if err != nil {
	// 	fmt.Println("error opening connections,", err)
	// 	return
	// }

	// // Register the messageCreate func as a callback for MessageCreate events.
	// dg.AddHandler(messageCreate)
	// dg.AddHandler(dumpMyOwnPresence)
	// dg.AddHandler(dumpAll)
	dg.AddHandler(channelCreate)
	dg.AddHandler(channelUpdate)

	// Open a websocket connection to Discord and begin listening.
	err = dg.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}

	// Wait here until CTRL-C or other term signal is received.
	fmt.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	// Cleanly close down the Discord session.
	// manager.StopAll()
	dg.Close()
}

func dumpAll(s *discordgo.Session, evt interface{}) {
	if _, ok := evt.(*discordgo.Event); !ok {
		// fmt.Printf("Inc event: %#v\n", evt)
	}
}
func dumpMyOwnPresence(s *discordgo.Session, evt *discordgo.PresenceUpdate) {
	if evt.Presence.User.ID == 105487308693757952 {
		serialized, _ := json.Marshal(evt)
		fmt.Println(string(serialized))
	}
}

func channelCreate(s *discordgo.Session, c *discordgo.ChannelCreate) {
	fmt.Printf("Channel create: %d - %d\n", c.GuildID, c.Channel.ID)
}

func channelUpdate(s *discordgo.Session, c *discordgo.ChannelUpdate) {
	fmt.Printf("Channel update: %d - %d\n", c.GuildID, c.Channel.ID)
}

// This function will be called (due to AddHandler above) every time a new
// message is created on any channel that the autenticated bot has access to.
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID != 105487308693757952 {
		return
	}

	if m.Content == "yaboi recon" {
		fmt.Println("Reconnecting...")
		err := s.GatewayManager.Reconnect(false)
		if err != nil {
			fmt.Println("Failed reconnecting")
		}
	}

	if m.Content == "yaboi joinvoice" {
		fmt.Println("joining cvoice")
		if vc != nil {
			fmt.Println("already in vc")
			return
		}

		channel, _ := s.State.Channel(m.ChannelID)
		g, _ := s.State.Guild(channel.GuildID)

		vcId := int64(0)
		for _, v := range g.VoiceStates {
			if v.UserID == m.Author.ID {
				vcId = v.ChannelID
				break
			}
		}

		if vcId == 0 {
			fmt.Println("Not in voice")
			return
		}

		var err error
		vc, err = s.GatewayManager.ChannelVoiceJoin(g.ID, vcId, true, true)
		if err != nil {
			fmt.Println("failed joining voice: ", err)
			return
		}
		fmt.Println("Joined voice")
	}

	if m.Content == "yaboi leavevoice" {
		if vc == nil {
			fmt.Println("Not in voice")
			return
		}

		err := vc.Disconnect()
		if err != nil {
			fmt.Println("failed leaving voice: ", err)
			return
		}
		vc = nil
	}

	if m.Content == "yaboi ping" {
		_, err := s.ChannelMessageSend(m.ChannelID, "pong")
		fmt.Println("pong: ", err)
	}

	// fmt.Println("\nReceived message my dude!\n")
	// // Ignore all messages created by the bot itself
	// // This isn't required in this specific example but it's a good practice.
	// if m.Author.ID == s.State.User.ID {
	// 	return
	// }
	// // If the message is "ping" reply with "Pong!"
	// if m.Content == "ping" {
	// 	s.ChannelMessageSend(m.ChannelID, "Pong!")
	// }

	// // If the message is "pong" reply with "Ping!"
	// if m.Content == "pong" {
	// 	s.ChannelMessageSend(m.ChannelID, "Ping!")
	// }
}

type LoggingTransport struct {
	Inner http.RoundTripper
}

var numberRemover = strings.NewReplacer(
	"0", "",
	"1", "",
	"2", "",
	"3", "",
	"4", "",
	"5", "",
	"6", "",
	"7", "",
	"8", "",
	"9", "")

func (t *LoggingTransport) RoundTrip(request *http.Request) (*http.Response, error) {

	inner := t.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}

	started := time.Now()

	code := 0
	resp, err := inner.RoundTrip(request)
	if resp != nil {
		code = resp.StatusCode
	}

	since := time.Since(started).Seconds() * 1000
	go func() {
		// path := numberRemover.Replace(request.URL.Path)
		fmt.Println("did request in: ", since, "c:", code, ", p:", request.URL.Path)

		// Statsd.Incr("discord.response.code."+strconv.Itoa(floored), nil, 1)
		// Statsd.Incr("discord.request.method."+request.Method, nil, 1)
	}()

	return resp, err
}
