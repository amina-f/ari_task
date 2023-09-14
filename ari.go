package main

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/CyCoreSystems/ari/v6"
	"github.com/CyCoreSystems/ari/v6/client/native"
)

var (
	calls        map[string]bool // The bool value is true for a call and false for a conference.
	switchToConf map[string]chan bool
	timeout      int64
)

const (
	serverIP   = "10.1.0.92"
	serverPort = "8088"
	username   = "asterisk"
	password   = "test123"
	stasisApp  = "myStasisApp"
)

func createBridgeID() string {
	rand.Seed(time.Now().UnixNano())
	length := 4
	randomStr := make([]byte, length)
	for i := 0; i < length; i++ {
		randomStr[i] = byte(65 + rand.Intn(25))
	}
	return string(randomStr)
}

func menu() {
	fmt.Println("Enter one of the available options:")
	fmt.Println("-> list 			(list all ongoing calls with their call IDs and participants)")
	fmt.Println("-> dial A B C ... 		(initiate a call between extentions A, B, C, etc.)")
	fmt.Println("-> join CALLID A B C ... 	(allow extentions A, B, C, etc. to join the call with ID CALLID)")
	fmt.Println("-> exit				(exit the application)")
}

func processInput(input string) []string {
	input = input[:len(input)-1]
	if input == "" {
		return nil
	}

	f := func(c rune) bool {
		return c == ' '
	}
	var inputFields []string = strings.FieldsFunc(input, f)

	switch inputFields[0] {
	case "dial":
		fallthrough
	case "join":
		if len(inputFields) < 3 {
			return nil
		}
	case "list":
		if len(inputFields) > 1 {
			return nil
		}
	}
	return inputFields
}

func callBridgeHandler(cl ari.Client, bh *ari.BridgeHandle, channelIDs *[]string) {
	chanEnteredBridge := bh.Subscribe(ari.Events.ChannelEnteredBridge)
	defer chanEnteredBridge.Cancel()
	chanLeftBridge := bh.Subscribe(ari.Events.ChannelLeftBridge)
	defer chanLeftBridge.Cancel()

	for {
		select {
		case <-switchToConf[bh.ID()]:
			go confBridgeHandler(cl, bh)
			return
		case e := <-chanEnteredBridge.Events():
			v := e.(*ari.ChannelEnteredBridge)
			numOfChans := len(v.Bridge.ChannelIDs)
			_, ok := calls[bh.ID()]
			if !ok && numOfChans == 2 {
				calls[bh.ID()] = true
				switchToConf[bh.ID()] = make(chan bool)
			}
		case e := <-chanLeftBridge.Events():
			v := e.(*ari.ChannelLeftBridge)
			cl.Channel().Hangup(v.Channel.Key, "normal")
			if len(v.Bridge.ChannelIDs) == 0 && len(*channelIDs) != 0 {
				index := 0
				if (*channelIDs)[0] == v.Channel.ID {
					index = 1
				}
				extToRemove := (*channelIDs)[index]
				*channelIDs = (*channelIDs)[0:0]
				ch := cl.Channel().Get(ari.NewKey("", extToRemove))
				ch.Hangup()
			} else if len(v.Bridge.ChannelIDs) == 1 {
				err := bh.RemoveChannel(v.Bridge.ChannelIDs[0])
				logErr(err)
				cl.Channel().Hangup(v.Keys()[len(v.Keys())-1], "normal")
			}
			delete(calls, v.Bridge.ID)
			delete(switchToConf, v.Bridge.ID)
			bh.Delete()
			return
		}
	}
}

func confBridgeHandler(cl ari.Client, bh *ari.BridgeHandle) {
	chanEnteredBridge := bh.Subscribe(ari.Events.ChannelEnteredBridge)
	defer chanEnteredBridge.Cancel()
	chanLeftBridge := bh.Subscribe(ari.Events.ChannelLeftBridge)
	defer chanLeftBridge.Cancel()

	for {
		select {
		case e := <-chanEnteredBridge.Events():
			v := e.(*ari.ChannelEnteredBridge)
			numOfChans := len(v.Bridge.ChannelIDs)
			_, ok := calls[bh.ID()]
			if !ok && numOfChans == 1 {
				calls[bh.ID()] = false
				switchToConf[bh.ID()] = make(chan bool)
			}
		case e := <-chanLeftBridge.Events():
			v := e.(*ari.ChannelLeftBridge)
			cl.Channel().Hangup(v.Channel.Key, "normal")
			if len(v.Bridge.ChannelIDs) == 0 {
				delete(calls, v.Bridge.ID)
				delete(switchToConf, v.Bridge.ID)
				bh.Delete()
				return
			}
		}
	}
}

func channelHandler(cl ari.Client, h *ari.ChannelHandle, bh *ari.BridgeHandle, extens *[]string) {
	stateChange := h.Subscribe(ari.Events.ChannelStateChange)
	defer stateChange.Cancel()
	channelHangupRequest := h.Subscribe(ari.Events.ChannelHangupRequest)
	defer channelHangupRequest.Cancel()

	for {
		select {
		case e := <-stateChange.Events():
			v := e.(*ari.ChannelStateChange)
			if v.Channel.State == "Up" {
				h.Answer()
				_, err := bh.Data()
				if err == nil {
					bh.AddChannel(h.ID())
				} else {
					h.Hangup()
				}
				return
			}
		case e := <-channelHangupRequest.Events():
			v := e.(*ari.ChannelHangupRequest)
			if len(*extens) > 2 && (*extens)[0] == "conference" {
				for i := 0; i < len(*extens); i++ {
					if (*extens)[i] == v.Channel.ID {
						(*extens)[i] = (*extens)[len(*extens)-1]
						(*extens) = (*extens)[0 : len(*extens)-1]
						break
					}
				}
			} else if len(*extens) == 2 && (*extens)[0] != "conference" {
				index := 0
				if (*extens)[0] == v.Channel.ID {
					index = 1
				}
				extToRemove := (*extens)[index]
				*extens = (*extens)[0:0]
				ch := cl.Channel().Get(ari.NewKey("", extToRemove))
				ch.Hangup()
				bh.Delete()
			} else {
				*extens = (*extens)[0:0]
				bh.Delete()
			}
			return
		}
	}
}

func createChannel(cl ari.Client, tech string, ext string, app string) (h *ari.ChannelHandle, err error) {
	h, err = cl.Channel().Create(nil, ari.ChannelCreateRequest{
		Endpoint: tech + "/" + ext,
		App:      app,
	})
	logErr(err)
	return
}

func createCall(cl ari.Client, ext string) (h *ari.ChannelHandle, err error) {
	h, err = createChannel(cl, "PJSIP", ext, stasisApp)
	logErr(err)
	err = h.Dial("", time.Duration(timeout))
	logErr(err)
	return
}

func dial(extens []string, cl ari.Client) {
	for _, ext := range extens {
		if !checkEndpointStatus("PJSIP", ext, cl) {
			fmt.Println(" << Cannot dial endpoint " + ext + ". Please try again.")
			return
		}
	}

	key := ari.NewKey("", createBridgeID())
	bh, err := cl.Bridge().Create(key, "mixing", key.ID)
	exitOnErr(err)

	channelIDs := make([]string, 0)
	if len(extens) > 2 {
		channelIDs = append(channelIDs, "conference")
	}
	for i := 0; i < len(extens); i++ {
		h, err := createCall(cl, extens[i])
		exitOnErr(err)
		channelIDs = append(channelIDs, h.ID())
		go channelHandler(cl, h, bh, &channelIDs)
	}

	if len(extens) == 2 {
		go callBridgeHandler(cl, bh, &channelIDs)
	} else {
		go confBridgeHandler(cl, bh)
	}
	return
}

func listCalls(cl ari.Client) {
	if len(calls) == 0 {
		fmt.Println(" << There are no ongoing calls at the moment.")
		return
	}

	for i, _ := range calls {
		exts := getBridgeParticipants(i, cl)
		fmt.Print(" << ", i, ": ")
		for _, v := range exts {
			fmt.Print(v, " ")
		}
		fmt.Print("\n")
	}
}

func joinCall(callID string, extens []string, cl ari.Client) {
	if len(calls) == 0 {
		fmt.Println(" << There are no ongoing calls at the moment.")
		return
	}

	_, ok := calls[callID]
	if !ok {
		fmt.Println(" << There are no calls matching the entered call ID.")
		return
	}

	for _, ext := range extens {
		if !checkEndpointStatus("PJSIP", ext, cl) {
			fmt.Println(" << Cannot dial endpoint " + ext + ". Please try again.")
			return
		}
	}

	if calls[callID] {
		switchToConf[callID] <- true 
		calls[callID] = false
	}

	bh := cl.Bridge().Get(ari.NewKey("", callID))
	data, err := bh.Data()
	exitOnErr(err)
	channelIDs := []string{"conference"}
	channelIDs = append(channelIDs, data.ChannelIDs...)
	for _, ext := range extens {
		h, err := createCall(cl, ext)
		logErr(err)
		channelIDs = append(channelIDs, h.ID())
		go channelHandler(cl, h, bh, &channelIDs)
	}
	return
}

func getBridgeParticipants(callID string, cl ari.Client) []string {
	bh := cl.Bridge().Get(ari.NewKey("", callID))
	data, err := bh.Data()
	exitOnErr(err)
	participants := make([]string, 0)
	for _, v := range data.ChannelIDs {
		h := cl.Channel().Get(ari.NewKey("", v))
		data, err := h.Data()
		exitOnErr(err)
		participants = append(participants, data.Accountcode)
	}
	return participants
}

func checkEndpointStatus(tech string, resource string, cl ari.Client) bool {
	data, _ := cl.Endpoint().Data(ari.NewEndpointKey(tech, resource))
	if data != nil && data.State == "online" {
		return true
	} else {
		return false
	}
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func logErr(err error) {
	if err != nil {
		fmt.Println(err)
	}
}

func main() {
	calls = make(map[string]bool)
	switchToConf = make(map[string]chan bool)
	timeout = 30e9
	cl, err := native.Connect(&native.Options{
		Application:  stasisApp,
		Username:     username,
		Password:     password,
		URL:          "http://" + serverIP + ":" + serverPort + "/ari",
		WebsocketURL: "ws://" + serverIP + ":" + serverPort + "/ari/events",
	})
	exitOnErr(err)
	defer cl.Close()

	fmt.Println("\nWelcome to the ARI CLI!")
	menu()
	var reader = bufio.NewReader(os.Stdin)

	for {
		fmt.Print("\n >> ")
		input, err := reader.ReadString('\n')
		exitOnErr(err)
		processedInput := processInput(input)
		var command string
		if processedInput == nil {
			command = ""
		} else {
			command = processedInput[0]
		}

		switch command {
		case "dial":
			dial(processedInput[1:], cl)
		case "list":
			listCalls(cl)
		case "join":
			joinCall(processedInput[1], processedInput[2:], cl)
		case "exit":
			fmt.Println(" << Goodbye. Thanks for all the fish!")
			cl.Close()
			break
		default:
			fmt.Println(" << Invalid input. Please try again.\n")
			menu()
		}
		if command == "exit" {
			break
		}
	}
	return
}
