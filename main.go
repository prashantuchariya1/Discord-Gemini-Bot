package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/generative-ai-go/genai"
	"github.com/joho/godotenv"
	"google.golang.org/api/option"
)

var (
	geminiClient *genai.Client
	chatSession  *genai.ChatSession
	ctx          context.Context
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	// Create Discord session
	discord, err := discordgo.New("Bot " + os.Getenv("DISCORD_BOT_TOKEN"))
	if err != nil {
		log.Fatal("Error creating Discord session:", err)
	}

	// Set bot avatar
	err = setBotAvatar(discord, "icon.png", "Go-Gemini-Bot")
	if err != nil {
		log.Println("Could not set bot avatar:", err)
	}

	// Create Gemini client
	ctx = context.Background()
	geminiClient, err = genai.NewClient(ctx, option.WithAPIKey(os.Getenv("GEMINI_API_KEY")))
	if err != nil {
		log.Fatal("Error creating Gemini client:", err)
	}
	defer geminiClient.Close()

	// Create chat model
	model := geminiClient.GenerativeModel("gemini-1.5-pro-latest")

	// Set response safety settings
	model.SafetySettings = []*genai.SafetySetting{
		{
			Category:  genai.HarmCategoryHarassment,
			Threshold: genai.HarmBlockNone,
		},
		{
			Category:  genai.HarmCategoryHateSpeech,
			Threshold: genai.HarmBlockNone,
		},
		{
			Category:  genai.HarmCategorySexuallyExplicit,
			Threshold: genai.HarmBlockNone,
		},
	}
	chatSession = model.StartChat()

	// Add message handler
	discord.AddHandler(messageHandler)

	// Add slash command handler
	discord.AddHandler(interactionHandler)

	// Open Discord session
	if err := discord.Open(); err != nil {
		log.Fatal("Cannot open the session:", err)
	}

	// Create slash command
	_, err = discord.ApplicationCommandCreate(discord.State.User.ID, "", &discordgo.ApplicationCommand{
		Name:        "clear",
		Description: "Clear the chat history with Gemini AI",
	})
	if err != nil {
		log.Fatal("Cannot create slash command:", err)
	}

	// Wait here until CTRL-C or other term signal is received
	fmt.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	// Cleanly close down the Discord session
	discord.Close()
}
func messageHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore bot's own messages
	if m.Author.ID == s.State.User.ID {
		return
	}

	userMessage := m.Content
	// Prepare parts for Gemini
	var parts []genai.Part

	// Check for attachments
	if len(m.Attachments) > 0 {
		for _, attachment := range m.Attachments {
			// Determine file type using MIME types
			isSupported := strings.HasPrefix(attachment.ContentType, "image/") ||
				strings.HasPrefix(attachment.ContentType, "video/") ||
				strings.HasPrefix(attachment.ContentType, "audio/") ||
				strings.Contains(attachment.ContentType, "pdf") ||
				strings.Contains(attachment.ContentType, "text/") ||
				strings.Contains(attachment.ContentType, "application/")

			if isSupported {
				// Download the file
				fileResp, err := http.Get(attachment.URL)
				if err != nil {
					log.Printf("Error downloading file: %v", err)
					continue
				}
				defer fileResp.Body.Close()

				// Read file bytes
				fileBytes, err := io.ReadAll(fileResp.Body)
				if err != nil {
					log.Printf("Error reading file bytes: %v", err)
					continue
				}

				// Use File API for all supported files
				uploadOpts := genai.UploadFileOptions{DisplayName: attachment.Filename}

				// Create a bytes.Reader from the file bytes
				fileReader := bytes.NewReader(fileBytes)

				uploadedFile, err := geminiClient.UploadFile(ctx, "", fileReader, &uploadOpts)
				if err != nil {
					log.Printf("Error uploading file: %v", err)
					continue
				}

				// Wait for processing (simple polling)
				for {
					fileStatus, err := geminiClient.GetFile(ctx, uploadedFile.Name)
					if err != nil {
						log.Printf("Error checking file status: %v", err)
						break
					}
					if fileStatus.State == genai.FileStateActive {
						parts = append(parts, genai.FileData{URI: fileStatus.URI})
						break
					}
					// Simple delay between checks
					time.Sleep(5 * time.Second)
				}
			}
		}
	}

	// Add text message to parts if not empty
	if userMessage != "" {
		parts = append(parts, genai.Text(userMessage))
	}

	// Ignore empty messages and no attachments
	if len(parts) == 0 {
		return
	}

	// Send typing indicator
	s.ChannelTyping(m.ChannelID)

	// Send message to Gemini
	resp, err := chatSession.SendMessage(ctx, parts...)
	if err != nil {
		errorMsg := fmt.Sprintf("Sorry, an error occurred: %v", err)
		s.ChannelMessageSend(m.ChannelID, errorMsg)
		log.Println("Gemini error:", err)
		return
	}

	// Extract and send response
	var responseText string
	for _, cand := range resp.Candidates {
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				responseText += fmt.Sprintf("%v", part)
			}
		}
	}

	// Send response
	if responseText != "" {
		// Split long messages if necessary
		for len(responseText) > 0 {
			// Determine message chunk size (Discord has a 2000 character limit)
			chunkSize := 2000
			if len(responseText) < chunkSize {
				chunkSize = len(responseText)
			}

			// Send message chunk
			chunk := responseText[:chunkSize]
			s.ChannelMessageSend(m.ChannelID, chunk)

			// Remove sent chunk
			responseText = responseText[chunkSize:]
		}
	} else {
		s.ChannelMessageSend(m.ChannelID, "I couldn't generate a response.")
	}
}

func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type == discordgo.InteractionApplicationCommand {
		switch i.ApplicationCommandData().Name {
		case "clear":
			clearChatHistory(s, i)
		}
	}
}

func clearChatHistory(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Restart the chat session to effectively clear the history
	model := geminiClient.GenerativeModel("gemini-1.5-pro-latest")
	chatSession = model.StartChat()

	// Respond to the slash command
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Chat history has been cleared!",
		},
	})
	if err != nil {
		log.Printf("Error responding to clear command: %v", err)
	}
}

// Function to set bot avatar
func setBotAvatar(s *discordgo.Session, avatarPath string, username string) error {
	// Read the avatar file
	avatarBytes, err := os.ReadFile(avatarPath)
	if err != nil {
		return fmt.Errorf("error reading avatar file: %v", err)
	}

	// Encode the avatar to base64
	avatarBase64 := base64.StdEncoding.EncodeToString(avatarBytes)
	avatarData := "data:image/png;base64," + avatarBase64

	// Update the bot's avatar
	_, err = s.UserUpdate(username, avatarData)
	if err != nil {
		return fmt.Errorf("error updating bot avatar: %v", err)
	}

	return nil
}
