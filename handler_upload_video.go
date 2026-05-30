package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	const maxMemory = 32 << 20
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}

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
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not the owner of this video", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get file from form", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse content type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Only mp4 videos are accepted", nil)
		return
	}

	tmp, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := io.Copy(tmp, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save temp file", err)
		return
	}

	aspect, err := getVideoAspectRatio(tmp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't determine aspect ratio", err)
		return
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek temp file", err)
		return
	}

	rnd := make([]byte, 16)
	if _, err := rand.Read(rnd); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate key", err)
		return
	}
	prefix := "other"
	switch aspect {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	}
	key := fmt.Sprintf("%s/%x.mp4", prefix, rnd)

	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        tmp,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload to S3", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)
	video.VideoURL = &videoURL

	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
    cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
    var stdout bytes.Buffer
    cmd.Stdout = &stdout

    if err := cmd.Run(); err != nil {
        return "", err
    }

    var result struct {
        Streams []struct {
            CodecType string `json:"codec_type"`
            Width     int    `json:"width"`
            Height    int    `json:"height"`
        } `json:"streams"`
    }
    if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
        return "", err
    }

    for _, stream := range result.Streams {
        if stream.CodecType != "video" {
            continue
        }
        if stream.Width == 0 || stream.Height == 0 {
            break
        }
        ratio := float64(stream.Width) / float64(stream.Height)
        if math.Abs(ratio-(16.0/9.0)) < 0.05 {
            return "16:9", nil
        }
        if math.Abs(ratio-(9.0/16.0)) < 0.05 {
            return "9:16", nil
        }
        return "other", nil
    }

    return "other", nil
}