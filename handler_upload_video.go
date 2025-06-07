package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video", err)
		return
	}

	if userID != video.UserID {
		respondWithError(w, http.StatusUnauthorized, "User not authorized to access video", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing media type", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type. Valid file types: .mp4", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error writing file to disk", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error resetting file pointer", err)
		return
	}

	directory := ""
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio", err)
		return
	}

	switch aspectRatio {
	case "16:9":
		directory = "landscape"
	case "9:16":
		directory = "portrait"
	default:
		directory = "other"
	}

	fileKey := getAssetPath(mediaType)
	fileKey = filepath.Join(directory, fileKey)
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(fileKey),
		Body:        tempFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading into s3", err)
		return
	}

	videoURL := cfg.getObjectURL(fileKey)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	// fmt.Println(filePath)
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	buffer := bytes.Buffer{}
	cmd.Stdout = &buffer

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffprobe error: %v", err)
	}

	var streams struct {
		Streams []struct {
			Width  int `json:"width,omitempty"`
			Height int `json:"height,omitempty"`
		} `json:"streams"`
	}
	err = json.Unmarshal(buffer.Bytes(), &streams)
	if err != nil {
		return "", fmt.Errorf("couldn't parse ffprobe output: %v", err)
	}

	if len(streams.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	width := streams.Streams[0].Width
	height := streams.Streams[0].Height

	if width == 16*height/9 {
		return "16:9", nil
	} else if height == 16*width/9 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}
