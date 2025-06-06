package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"speach2text/llm"
	"strings"
	"time"
)

type Config struct {
	Bucket string `json:"bucket"`
}

type TranscriptResponse struct {
	Results struct {
		Transcripts []struct {
			Transcript string `json:"transcript"`
		} `json:"transcripts"`
	} `json:"results"`
}

func readConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var config Config
	err = json.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func convertM4AToMP3(inputFile string) (string, error) {
	outputFile := strings.TrimSuffix(inputFile, ".m4a") + ".mp3"
	fmt.Printf("Converting %s to %s...\n", inputFile, outputFile)
	cmd := exec.Command("ffmpeg", "-i", inputFile, outputFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFile, nil
}

func copyToS3(file, bucket string) (string, error) {
	s3Key := "summary/" + filepath.Base(file)
	dest := fmt.Sprintf("s3://%s/%s", bucket, s3Key)
	fmt.Printf("Copying %s to %s...\n", file, dest)
	cmd := exec.Command("aws", "s3", "cp", file, dest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return s3Key, nil
}

func startTranscribeJob(bucket, mp3Key string) (string, error) {
	jobName := strings.TrimSuffix(filepath.Base(mp3Key), ".mp3") + "-DMIN-" + fmt.Sprintf("%d", time.Now().Unix())
	mediaURI := fmt.Sprintf("s3://%s/%s", bucket, mp3Key)
	fmt.Printf("Starting transcription job '%s' for %s...\n", jobName, mediaURI)
	outputKey := fmt.Sprintf("summary/output/%s.json", jobName)
	cmd := exec.Command("aws", "transcribe", "start-transcription-job",
		"--transcription-job-name", jobName,
		"--language-code", "en-US",
		"--media-sample-rate-hertz", "48000",
		"--media-format", "mp3",
		"--media", fmt.Sprintf("MediaFileUri=%s", mediaURI),
		"--output-bucket-name", bucket,
		"--output-key", outputKey,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return jobName, nil
}

func waitForTranscribeJob(jobName string) error {
	fmt.Printf("Waiting for transcription job '%s' to complete...\n", jobName)
	for {
		cmd := exec.Command("aws", "transcribe", "get-transcription-job",
			"--transcription-job-name", jobName)
		output, err := cmd.Output()
		if err != nil {
			return err
		}
		var resp map[string]any
		err = json.Unmarshal(output, &resp)
		if err != nil {
			return err
		}
		job, ok := resp["TranscriptionJob"].(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected response format")
		}
		status, ok := job["TranscriptionJobStatus"].(string)
		if !ok {
			return fmt.Errorf("unexpected status format")
		}
		fmt.Printf("Current status: %s\n", status)
		if status == "COMPLETED" {
			break
		} else if status == "FAILED" {
			return fmt.Errorf("transcription job failed")
		}
		time.Sleep(10 * time.Second)
	}
	return nil
}

func getTranscriptText(jobName, bucket string) (string, error) {
	s3Key := fmt.Sprintf("summary/output/%s.json", jobName)
	localFile := s3Key
	if err := os.MkdirAll(filepath.Dir(localFile), os.ModePerm); err != nil {
		return "", err
	}
	s3Path := fmt.Sprintf("s3://%s/%s", bucket, s3Key)
	fmt.Printf("Fetching transcription result from %s...\n", s3Path)
	cmd := exec.Command("aws", "s3", "cp", s3Path, localFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(localFile)
	if err != nil {
		return "", err
	}
	var transcriptResp TranscriptResponse
	if err := json.Unmarshal(data, &transcriptResp); err != nil {
		return "", err
	}
	if len(transcriptResp.Results.Transcripts) == 0 {
		return "", fmt.Errorf("no transcript found")
	}
	return transcriptResp.Results.Transcripts[0].Transcript, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: transcription_pipeline <input-file.m4a>")
		os.Exit(1)
	}
	inputFile := os.Args[1]
	config, err := readConfig("config.json")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}
	bucket := config.Bucket

	mp3File, err := convertM4AToMP3(inputFile)
	if err != nil {
		log.Fatalf("Error converting file: %v", err)
	}

	mp3Key, err := copyToS3(mp3File, bucket)
	if err != nil {
		log.Fatalf("Error copying file to S3: %v", err)
	}

	jobName, err := startTranscribeJob(bucket, mp3Key)
	if err != nil {
		log.Fatalf("Error starting transcription job: %v", err)
	}

	if err := waitForTranscribeJob(jobName); err != nil {
		log.Fatalf("Error waiting for transcription job: %v", err)
	}

	transcript, err := getTranscriptText(jobName, bucket)
	if err != nil {
		log.Fatalf("Error getting transcript text: %v", err)
	}

	promptData, err := os.ReadFile("prompt.txt")
	if err != nil {
		log.Fatalf("Error reading prompt.txt: %v", err)
	}
	fullPrompt := string(promptData) + "\n" + transcript

	bedrockResult, err := llm.CallBedrock(fullPrompt)
	if err != nil {
		log.Fatalf("Error calling Bedrock: %v", err)
	}

	err = os.WriteFile("result.txt", []byte(bedrockResult), 0644)
	if err != nil {
		log.Fatalf("Error writing result.txt: %v", err)
	}
	fmt.Println("Done. Result written to result.txt")
}
