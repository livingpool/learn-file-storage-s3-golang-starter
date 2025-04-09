package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"os"

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

	// Reset the tempFile's file pointer to the beginning
	tempFile.Seek(0, io.SeekStart)

	// Put the object in S3
	randBytes := make([]byte, 32)
	if _, err := rand.Read(randBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Generating rand bytes failed", err)
		return
	}
	fileKey := getAssetPath(hex.EncodeToString(randBytes), mediaType)
	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        tempFile,
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
