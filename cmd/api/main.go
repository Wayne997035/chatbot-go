package main

func main() {

	server, err := InitializeChatbot()
	if err != nil {
		return
	}

	if err = server.Start(":8080"); err != nil {
		return
	}
}
