package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30 // 1 GB
	http.MaxBytesReader(w, r.Body, uploadLimit)

	// Get video id
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate the user
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

	// Get the video metadata from the database, if the user is not the video owner, return 401
	vid, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get video", err)
		return
	}
	if vid.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "The authenticated user is not the video owner", nil)
		return
	}

	// Parse the uploaded video file from the form data
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse form file", err)
		return
	}
	defer file.Close()

	// Validate the uploaded video file to ensure it's an MP4 video
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	// Save the uploaded file to a temporary file on disk
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temporary file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	io.Copy(tempFile, file)

	// Pre-process the video for fast start (by moving the moov atom to the start)
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}
	defer processedFile.Close()

	// Get the video aspect ratio of the video from the tempFile
	ratio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse video aspect ratio", err)
		return
	}

	// Reset the tempFile's file pointer to the beginning
	tempFile.Seek(0, io.SeekStart)

	// Put the object in S3
	randBytes := make([]byte, 32)
	if _, err := rand.Read(randBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Generating rand bytes failed", err)
		return
	}
	fileKey := getAssetPath(hex.EncodeToString(randBytes), mediaType)

	switch ratio {
	case "16:9":
		fileKey = "landscape/" + fileKey
	case "9:16":
		fileKey = "portrait/" + fileKey
	default:
		fileKey = "other/" + fileKey
	}

	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusFailedDependency, "Unable to upload to S3", err)
		return
	}

	// Update the VideoURL of the video record in the database with the S3 bucket and key
	url := cfg.getObjectURL(fileKey)
	vid.VideoURL = &url

	if err := cfg.db.UpdateVideo(vid); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, vid)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams", filePath,
	)
	buf := bytes.NewBuffer([]byte{})
	cmd.Stdout = buf

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffprobe error: %v", err)
	}

	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	err = json.Unmarshal(buf.Bytes(), &output)
	if err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %v", err)
	}
	if len(output.Streams) == 0 {
		return "", fmt.Errorf("Parsed video stream is empty")
	}

	width := output.Streams[0].Width
	height := output.Streams[0].Height
	ratio := float64(width) / float64(height)

	const horizontal = 16.0 / 9.0
	const vertical = 9.0 / 16.0

	if math.Abs(ratio-horizontal) <= 0.01 {
		return "16:9", nil
	} else if math.Abs(ratio-vertical) <= 0.01 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(inputPath string) (string, error) {
	outputPath := inputPath + ".processing"

	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4", outputPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(outputPath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return outputPath, nil
}
