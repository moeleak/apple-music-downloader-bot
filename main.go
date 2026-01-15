package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apputils "main/utils"
	"main/utils/ampapi"
	"main/utils/lyrics"
	"main/utils/runv2"
	"main/utils/runv3"
	"main/utils/structs"
	"main/utils/task"

	"github.com/fatih/color"
	"github.com/grafov/m3u8"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/pflag"
	"github.com/zhaarey/go-mp4tag"
	"gopkg.in/yaml.v2"
)

var (
	forbiddenNames = regexp.MustCompile(`[/\\<>:"|?*]`)
	dl_atmos       bool
	dl_aac         bool
	dl_select      bool
	dl_song        bool
	artist_select  bool
	debug_mode     bool
	alac_max       *int
	atmos_max      *int
	mv_max         *int
	mv_audio_type  *string
	aac_type       *string
	Config         structs.ConfigSet
	counter        structs.Counter
	okDict         = make(map[string][]int)
	lastDownloadedPaths []string
	activeProgress func(phase string, done, total int64)
	downloadedMetaMu sync.Mutex
	downloadedMeta   = make(map[string]AudioMeta)
	searchMetaMu     sync.Mutex
	searchMetaByID   = make(map[string]AudioMeta)
)

type AudioMeta struct {
	TrackID   string
	Title     string
	Performer string
	DurationMillis int64
}

type CachedAudio struct {
	FileID    string    `json:"file_id"`
	FileSize  int64     `json:"file_size"`
	Compressed bool     `json:"compressed"`
	Format    string    `json:"format,omitempty"`
	SizeBytes  int64    `json:"size_bytes,omitempty"`
	BitrateKbps float64 `json:"bitrate_kbps,omitempty"`
	DurationMillis int64 `json:"duration_millis,omitempty"`
	Title     string    `json:"title,omitempty"`
	Performer string    `json:"performer,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type telegramCacheFile struct {
	Version int                    `json:"version"`
	Items   map[string]CachedAudio `json:"items"`
}

func loadConfig() error {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		return err
	}
	err = yaml.Unmarshal(data, &Config)
	if err != nil {
		return err
	}
	if len(Config.Storefront) != 2 {
		Config.Storefront = "us"
	}
	return nil
}

func recordDownloadedTrack(track *task.Track) {
	if track == nil || track.SavePath == "" {
		return
	}
	lastDownloadedPaths = append(lastDownloadedPaths, track.SavePath)
	meta := AudioMeta{
		TrackID:   strings.TrimSpace(track.ID),
		Title:     strings.TrimSpace(track.Resp.Attributes.Name),
		Performer: strings.TrimSpace(track.Resp.Attributes.ArtistName),
		DurationMillis: int64(track.Resp.Attributes.DurationInMillis),
	}
	if meta.TrackID != "" {
		if override, ok := popSearchMeta(meta.TrackID); ok {
			if override.Title != "" {
				meta.Title = override.Title
			}
			if override.Performer != "" {
				meta.Performer = override.Performer
			}
		}
	}
	if meta.Title != "" || meta.Performer != "" {
		downloadedMetaMu.Lock()
		downloadedMeta[track.SavePath] = meta
		downloadedMetaMu.Unlock()
	}
}

func getDownloadedMeta(path string) (AudioMeta, bool) {
	downloadedMetaMu.Lock()
	defer downloadedMetaMu.Unlock()
	meta, ok := downloadedMeta[path]
	return meta, ok
}

func setSearchMeta(trackID string, title string, performer string) {
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return
	}
	meta := AudioMeta{
		TrackID:   trackID,
		Title:     strings.TrimSpace(title),
		Performer: strings.TrimSpace(performer),
	}
	if meta.Title == "" && meta.Performer == "" {
		return
	}
	searchMetaMu.Lock()
	searchMetaByID[trackID] = meta
	searchMetaMu.Unlock()
}

func popSearchMeta(trackID string) (AudioMeta, bool) {
	searchMetaMu.Lock()
	defer searchMetaMu.Unlock()
	meta, ok := searchMetaByID[trackID]
	if ok {
		delete(searchMetaByID, trackID)
	}
	return meta, ok
}

func LimitString(s string) string {
	if len([]rune(s)) > Config.LimitMax {
		return string([]rune(s)[:Config.LimitMax])
	}
	return s
}

func isInArray(arr []int, target int) bool {
	for _, num := range arr {
		if num == target {
			return true
		}
	}
	return false
}

func fileExists(path string) (bool, error) {
	f, err := os.Stat(path)
	if err == nil {
		return !f.IsDir(), nil
	} else if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func checkUrl(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/album|\/album\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlMv(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/music-video|\/music-video\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlSong(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/song|\/song\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func checkUrlPlaylist(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/playlist|\/playlist\/.+))\/(?:id)?(pl\.[\w-]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

func checkUrlStation(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music)\.apple\.com\/(\w{2})(?:\/station|\/station\/.+))\/(?:id)?(ra\.[\w-]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}

func checkUrlArtist(url string) (string, string) {
	pat := regexp.MustCompile(`^(?:https:\/\/(?:beta\.music|music|classical\.music)\.apple\.com\/(\w{2})(?:\/artist|\/artist\/.+))\/(?:id)?(\d[^\D]+)(?:$|\?)`)
	matches := pat.FindAllStringSubmatch(url, -1)

	if matches == nil {
		return "", ""
	} else {
		return matches[0][1], matches[0][2]
	}
}
func getUrlSong(songUrl string, token string) (string, error) {
	storefront, songId := checkUrlSong(songUrl)
	manifest, err := ampapi.GetSongResp(storefront, songId, Config.Language, token)
	if err != nil {
		fmt.Println("\u26A0 Failed to get manifest:", err)
		counter.NotSong++
		return "", err
	}
	albumId := manifest.Data[0].Relationships.Albums.Data[0].ID
	songAlbumUrl := fmt.Sprintf("https://music.apple.com/%s/album/1/%s?i=%s", storefront, albumId, songId)
	return songAlbumUrl, nil
}
func getUrlArtistName(artistUrl string, token string) (string, string, error) {
	storefront, artistId := checkUrlArtist(artistUrl)
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s", storefront, artistId), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Origin", "https://music.apple.com")
	query := url.Values{}
	query.Set("l", Config.Language)
	req.URL.RawQuery = query.Encode()
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		return "", "", errors.New(do.Status)
	}
	obj := new(structs.AutoGeneratedArtist)
	err = json.NewDecoder(do.Body).Decode(&obj)
	if err != nil {
		return "", "", err
	}
	return obj.Data[0].Attributes.Name, obj.Data[0].ID, nil
}

func checkArtist(artistUrl string, token string, relationship string) ([]string, error) {
	storefront, artistId := checkUrlArtist(artistUrl)
	Num := 0
	//id := 1
	var args []string
	var urls []string
	var options [][]string
	for {
		req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/artists/%s/%s?limit=100&offset=%d&l=%s", storefront, artistId, relationship, Num, Config.Language), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		req.Header.Set("Origin", "https://music.apple.com")
		do, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer do.Body.Close()
		if do.StatusCode != http.StatusOK {
			return nil, errors.New(do.Status)
		}
		obj := new(structs.AutoGeneratedArtist)
		err = json.NewDecoder(do.Body).Decode(&obj)
		if err != nil {
			return nil, err
		}
		for _, album := range obj.Data {
			options = append(options, []string{album.Attributes.Name, album.Attributes.ReleaseDate, album.ID, album.Attributes.URL})
		}
		Num = Num + 100
		if len(obj.Next) == 0 {
			break
		}
	}
	sort.Slice(options, func(i, j int) bool {
		// 将日期字符串解析为 time.Time 类型进行比较
		dateI, _ := time.Parse("2006-01-02", options[i][1])
		dateJ, _ := time.Parse("2006-01-02", options[j][1])
		return dateI.Before(dateJ) // 返回 true 表示 i 在 j 前面
	})

	table := tablewriter.NewWriter(os.Stdout)
	if relationship == "albums" {
		table.SetHeader([]string{"", "Album Name", "Date", "Album ID"})
	} else if relationship == "music-videos" {
		table.SetHeader([]string{"", "MV Name", "Date", "MV ID"})
	}
	table.SetRowLine(false)
	table.SetHeaderColor(tablewriter.Colors{},
		tablewriter.Colors{tablewriter.FgRedColor, tablewriter.Bold},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})

	table.SetColumnColor(tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgRedColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgBlackColor})
	for i, v := range options {
		urls = append(urls, v[3])
		options[i] = append([]string{fmt.Sprint(i + 1)}, v[:3]...)
		table.Append(options[i])
	}
	table.Render()
	if artist_select {
		fmt.Println("You have selected all options:")
		return urls, nil
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Please select from the " + relationship + " options above (multiple options separated by commas, ranges supported, or type 'all' to select all)")
	cyanColor := color.New(color.FgCyan)
	cyanColor.Print("Enter your choice: ")
	input, _ := reader.ReadString('\n')

	input = strings.TrimSpace(input)
	if input == "all" {
		fmt.Println("You have selected all options:")
		return urls, nil
	}

	selectedOptions := [][]string{}
	parts := strings.Split(input, ",")
	for _, part := range parts {
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			selectedOptions = append(selectedOptions, rangeParts)
		} else {
			selectedOptions = append(selectedOptions, []string{part})
		}
	}

	fmt.Println("You have selected the following options:")
	for _, opt := range selectedOptions {
		if len(opt) == 1 {
			num, err := strconv.Atoi(opt[0])
			if err != nil {
				fmt.Println("Invalid option:", opt[0])
				continue
			}
			if num > 0 && num <= len(options) {
				fmt.Println(options[num-1])
				args = append(args, urls[num-1])
			} else {
				fmt.Println("Option out of range:", opt[0])
			}
		} else if len(opt) == 2 {
			start, err1 := strconv.Atoi(opt[0])
			end, err2 := strconv.Atoi(opt[1])
			if err1 != nil || err2 != nil {
				fmt.Println("Invalid range:", opt)
				continue
			}
			if start < 1 || end > len(options) || start > end {
				fmt.Println("Range out of range:", opt)
				continue
			}
			for i := start; i <= end; i++ {
				fmt.Println(options[i-1])
				args = append(args, urls[i-1])
			}
		} else {
			fmt.Println("Invalid option:", opt)
		}
	}
	return args, nil
}

func writeCover(sanAlbumFolder, name string, url string) (string, error) {
	originalUrl := url
	var ext string
	var covPath string
	if Config.CoverFormat == "original" {
		ext = strings.Split(url, "/")[len(strings.Split(url, "/"))-2]
		ext = ext[strings.LastIndex(ext, ".")+1:]
		covPath = filepath.Join(sanAlbumFolder, name+"."+ext)
	} else {
		covPath = filepath.Join(sanAlbumFolder, name+"."+Config.CoverFormat)
	}
	exists, err := fileExists(covPath)
	if err != nil {
		fmt.Println("Failed to check if cover exists.")
		return "", err
	}
	if exists {
		_ = os.Remove(covPath)
	}
	if Config.CoverFormat == "png" {
		re := regexp.MustCompile(`\{w\}x\{h\}`)
		parts := re.Split(url, 2)
		url = parts[0] + "{w}x{h}" + strings.Replace(parts[1], ".jpg", ".png", 1)
	}
	url = strings.Replace(url, "{w}x{h}", Config.CoverSize, 1)
	if Config.CoverFormat == "original" {
		url = strings.Replace(url, "is1-ssl.mzstatic.com/image/thumb", "a5.mzstatic.com/us/r1000/0", 1)
		url = url[:strings.LastIndex(url, "/")]
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	do, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer do.Body.Close()
	if do.StatusCode != http.StatusOK {
		if Config.CoverFormat == "original" {
			fmt.Println("Failed to get cover, falling back to " + ext + " url.")
			splitByDot := strings.Split(originalUrl, ".")
			last := splitByDot[len(splitByDot)-1]
			fallback := originalUrl[:len(originalUrl)-len(last)] + ext
			fallback = strings.Replace(fallback, "{w}x{h}", Config.CoverSize, 1)
			fmt.Println("Fallback URL:", fallback)
			req, err = http.NewRequest("GET", fallback, nil)
			if err != nil {
				fmt.Println("Failed to create request for fallback url.")
				return "", err
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
			do, err = http.DefaultClient.Do(req)
			if err != nil {
				fmt.Println("Failed to get cover from fallback url.")
				return "", err
			}
			defer do.Body.Close()
			if do.StatusCode != http.StatusOK {
				fmt.Println(fallback)
				return "", errors.New(do.Status)
			}
		} else {
			return "", errors.New(do.Status)
		}
	}
	f, err := os.Create(covPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = io.Copy(f, do.Body)
	if err != nil {
		return "", err
	}
	return covPath, nil
}

func writeLyrics(sanAlbumFolder, filename string, lrc string) error {
	lyricspath := filepath.Join(sanAlbumFolder, filename)
	f, err := os.Create(lyricspath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(lrc)
	if err != nil {
		return err
	}
	return nil
}

func contains(slice []string, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

func setDlFlags(quality string) {
	dl_atmos = false
	dl_aac = false

	switch quality {
	case "atmos":
		dl_atmos = true
		fmt.Println("Quality set to: Dolby Atmos")
	case "aac":
		dl_aac = true
		*aac_type = "aac"
		fmt.Println("Quality set to: High-Quality (AAC)")
	case "alac":
		fmt.Println("Quality set to: Lossless (ALAC)")
	}
}

func handleSearch(searchType string, queryParts []string, token string) (string, error) {
	selection, err := apputils.HandleSearch(searchType, queryParts, token, Config.Storefront, Config.Language)
	if err != nil {
		return "", err
	}
	if selection == nil || selection.URL == "" {
		return "", nil
	}
	if selection.IsSong {
		dl_song = true
	}
	if selection.Quality != "" && selection.Quality != "default" {
		setDlFlags(selection.Quality)
	}
	return selection.URL, nil
}

func convertIfNeeded(track *task.Track, lrc string) {
	coverPath := ""
	if strings.EqualFold(Config.ConvertFormat, "flac") && track.SaveDir != "" {
		coverPath = findCoverFile(track.SaveDir)
	}
	apputils.ConvertIfNeeded(track, lrc, &Config, coverPath, activeProgress)
}


func ripTrack(track *task.Track, token string, mediaUserToken string) {
	var err error
	counter.Total++
	fmt.Printf("Track %d of %d: %s\n", track.TaskNum, track.TaskTotal, track.Type)

	//提前获取到的播放列表下track所在的专辑信息
	if track.PreType == "playlists" && Config.UseSongInfoForPlaylist {
		track.GetAlbumData(token)
	}

	//mv dl dev
	if track.Type == "music-videos" {
		if len(mediaUserToken) <= 50 {
			fmt.Println("meida-user-token is not set, skip MV dl")
			counter.Success++
			return
		}
		if _, err := exec.LookPath("mp4decrypt"); err != nil {
			fmt.Println("mp4decrypt is not found, skip MV dl")
			counter.Success++
			return
		}
		err := mvDownloader(track.ID, track.SaveDir, token, track.Storefront, mediaUserToken, track)
		if err != nil {
			fmt.Println("\u26A0 Failed to dl MV:", err)
			counter.Error++
			return
		}
		counter.Success++
		return
	}

	needDlAacLc := false
	if dl_aac && Config.AacType == "aac-lc" {
		needDlAacLc = true
	}
	if track.WebM3u8 == "" && !needDlAacLc {
		if dl_atmos {
			fmt.Println("Unavailable")
			counter.Unavailable++
			return
		}
		fmt.Println("Unavailable, trying to dl aac-lc")
		needDlAacLc = true
	}
	needCheck := false

	if Config.GetM3u8Mode == "all" {
		needCheck = true
	} else if Config.GetM3u8Mode == "hires" && contains(track.Resp.Attributes.AudioTraits, "hi-res-lossless") {
		needCheck = true
	}
	var EnhancedHls_m3u8 string
	if needCheck && !needDlAacLc {
		EnhancedHls_m3u8, _ = checkM3u8(track.ID, "song")
		if strings.HasSuffix(EnhancedHls_m3u8, ".m3u8") {
			track.DeviceM3u8 = EnhancedHls_m3u8
			track.M3u8 = EnhancedHls_m3u8
		}
	}
	var Quality string
	if strings.Contains(Config.SongFileFormat, "Quality") {
		if dl_atmos {
			Quality = fmt.Sprintf("%dKbps", Config.AtmosMax-2000)
		} else if needDlAacLc {
			Quality = "256Kbps"
		} else {
			_, Quality, err = extractMedia(track.M3u8, true)
			if err != nil {
				fmt.Println("Failed to extract quality from manifest.\n", err)
				counter.Error++
				return
			}
		}
	}
	track.Quality = Quality

	stringsToJoin := []string{}
	if track.Resp.Attributes.IsAppleDigitalMaster {
		if Config.AppleMasterChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.AppleMasterChoice)
		}
	}
	if track.Resp.Attributes.ContentRating == "explicit" {
		if Config.ExplicitChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.ExplicitChoice)
		}
	}
	if track.Resp.Attributes.ContentRating == "clean" {
		if Config.CleanChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.CleanChoice)
		}
	}
	Tag_string := strings.Join(stringsToJoin, " ")

	songName := strings.NewReplacer(
		"{SongId}", track.ID,
		"{SongNumer}", fmt.Sprintf("%02d", track.TaskNum),
		"{SongName}", LimitString(track.Resp.Attributes.Name),
		"{ArtistName}", LimitString(track.Resp.Attributes.ArtistName),
		"{DiscNumber}", fmt.Sprintf("%0d", track.Resp.Attributes.DiscNumber),
		"{TrackNumber}", fmt.Sprintf("%0d", track.Resp.Attributes.TrackNumber),
		"{Quality}", Quality,
		"{Tag}", Tag_string,
		"{Codec}", track.Codec,
	).Replace(Config.SongFileFormat)
	fmt.Println(songName)
	filename := fmt.Sprintf("%s.m4a", forbiddenNames.ReplaceAllString(songName, "_"))
	track.SaveName = filename
	trackPath := filepath.Join(track.SaveDir, track.SaveName)
	lrcFilename := fmt.Sprintf("%s.%s", forbiddenNames.ReplaceAllString(songName, "_"), Config.LrcFormat)

	// Determine possible post-conversion target file (so we can skip re-download)
	var convertedPath string
	conversionEnabled := Config.ConvertAfterDownload &&
		Config.ConvertFormat != "" &&
		strings.ToLower(Config.ConvertFormat) != "copy"
	considerConverted := false
	if conversionEnabled {
		convertedPath = strings.TrimSuffix(trackPath, filepath.Ext(trackPath)) + "." + strings.ToLower(Config.ConvertFormat)
		if !Config.ConvertKeepOriginal {
			considerConverted = true
		}
	}
	//get lrc
	var lrc string = ""
	if Config.EmbedLrc || Config.SaveLrcFile {
		lrcStr, err := lyrics.Get(track.Storefront, track.ID, Config.LrcType, Config.Language, Config.LrcFormat, token, mediaUserToken)
		if err != nil {
			fmt.Println(err)
		} else {
			if Config.SaveLrcFile {
				err := writeLyrics(track.SaveDir, lrcFilename, lrcStr)
				if err != nil {
					fmt.Printf("Failed to write lyrics")
				}
			}
			if Config.EmbedLrc {
				lrc = lrcStr
			}
		}
	}

	// Existence check now considers converted output (if original was deleted)
	existsOriginal, err := fileExists(trackPath)
	if err != nil {
		fmt.Println("Failed to check if track exists.")
	}
	if existsOriginal {
		fmt.Println("Track already exists locally.")
		track.SavePath = trackPath
		track.SaveName = filepath.Base(trackPath)
		if conversionEnabled {
			if considerConverted {
				existsConverted, err2 := fileExists(convertedPath)
				if err2 == nil && existsConverted {
					track.SavePath = convertedPath
					track.SaveName = filepath.Base(convertedPath)
				} else {
					convertIfNeeded(track, lrc)
				}
			} else {
				convertIfNeeded(track, lrc)
			}
		}
		recordDownloadedTrack(track)
		counter.Success++
		okDict[track.PreID] = append(okDict[track.PreID], track.TaskNum)
		return
	}
	if considerConverted {
		existsConverted, err2 := fileExists(convertedPath)
		if err2 == nil && existsConverted {
			fmt.Println("Converted track already exists locally.")
			track.SavePath = convertedPath
			track.SaveName = filepath.Base(convertedPath)
			recordDownloadedTrack(track)
			counter.Success++
			okDict[track.PreID] = append(okDict[track.PreID], track.TaskNum)
			return
		}
	}

	if needDlAacLc {
		if len(mediaUserToken) <= 50 {
			fmt.Println("Invalid media-user-token")
			counter.Error++
			return
		}
		_, err := runv3.Run(track.ID, trackPath, token, mediaUserToken, false, "", activeProgress)
		if err != nil {
			fmt.Println("Failed to dl aac-lc:", err)
			if err.Error() == "Unavailable" {
				counter.Unavailable++
				return
			}
			counter.Error++
			return
		}
	} else {
		trackM3u8Url, _, err := extractMedia(track.M3u8, false)
		if err != nil {
			fmt.Println("\u26A0 Failed to extract info from manifest:", err)
			counter.Unavailable++
			return
		}
		//边下载边解密
		err = runv2.Run(track.ID, trackM3u8Url, trackPath, Config, activeProgress)
		if err != nil {
			fmt.Println("Failed to run v2:", err)
			counter.Error++
			return
		}
	}
	//这里利用MP4box将fmp4转化为mp4，并添加ilst box与cover，方便后面的mp4tag添加更多自定义标签
	tags := []string{
		"tool=",
		"artist=AppleMusic",
	}
	if Config.EmbedCover {
		if (strings.Contains(track.PreID, "pl.") || strings.Contains(track.PreID, "ra.")) && Config.DlAlbumcoverForPlaylist {
			track.CoverPath, err = writeCover(track.SaveDir, track.ID, track.Resp.Attributes.Artwork.URL)
			if err != nil {
				fmt.Println("Failed to write cover.")
			}
		}
		tags = append(tags, fmt.Sprintf("cover=%s", track.CoverPath))
	}
	tagsString := strings.Join(tags, ":")
	cmd := exec.Command("MP4Box", "-itags", tagsString, trackPath)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Embed failed: %v\n", err)
		counter.Error++
		return
	}
	if (strings.Contains(track.PreID, "pl.") || strings.Contains(track.PreID, "ra.")) && Config.DlAlbumcoverForPlaylist {
		if err := os.Remove(track.CoverPath); err != nil {
			fmt.Printf("Error deleting file: %s\n", track.CoverPath)
			counter.Error++
			return
		}
	}
	track.SavePath = trackPath
	err = writeMP4Tags(track, lrc)
	if err != nil {
		fmt.Println("\u26A0 Failed to write tags in media:", err)
		counter.Unavailable++
		return
	}

	// CONVERSION FEATURE hook
	convertIfNeeded(track, lrc)

	recordDownloadedTrack(track)
	counter.Success++
	okDict[track.PreID] = append(okDict[track.PreID], track.TaskNum)
}

func ripStation(albumId string, token string, storefront string, mediaUserToken string) error {
	station := task.NewStation(storefront, albumId)
	err := station.GetResp(mediaUserToken, token, Config.Language)
	if err != nil {
		return err
	}
	fmt.Println(" -", station.Type)
	meta := station.Resp

	var Codec string
	if dl_atmos {
		Codec = "ATMOS"
	} else if dl_aac {
		Codec = "AAC"
	} else {
		Codec = "ALAC"
	}
	station.Codec = Codec
	var singerFoldername string
	if Config.ArtistFolderFormat != "" {
		singerFoldername = strings.NewReplacer(
			"{ArtistName}", "Apple Music Station",
			"{ArtistId}", "",
			"{UrlArtistName}", "Apple Music Station",
		).Replace(Config.ArtistFolderFormat)
		if strings.HasSuffix(singerFoldername, ".") {
			singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
		}
		singerFoldername = strings.TrimSpace(singerFoldername)
		fmt.Println(singerFoldername)
	}
	singerFolder := filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	if dl_atmos {
		singerFolder = filepath.Join(Config.AtmosSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	if dl_aac {
		singerFolder = filepath.Join(Config.AacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	os.MkdirAll(singerFolder, os.ModePerm)
	station.SaveDir = singerFolder

	playlistFolder := strings.NewReplacer(
		"{ArtistName}", "Apple Music Station",
		"{PlaylistName}", LimitString(station.Name),
		"{PlaylistId}", station.ID,
		"{Quality}", "",
		"{Codec}", Codec,
		"{Tag}", "",
	).Replace(Config.PlaylistFolderFormat)
	if strings.HasSuffix(playlistFolder, ".") {
		playlistFolder = strings.ReplaceAll(playlistFolder, ".", "")
	}
	playlistFolder = strings.TrimSpace(playlistFolder)
	playlistFolderPath := filepath.Join(singerFolder, forbiddenNames.ReplaceAllString(playlistFolder, "_"))
	os.MkdirAll(playlistFolderPath, os.ModePerm)
	station.SaveName = playlistFolder
	fmt.Println(playlistFolder)

	covPath, err := writeCover(playlistFolderPath, "cover", meta.Data[0].Attributes.Artwork.URL)
	if err != nil {
		fmt.Println("Failed to write cover.")
	}
	station.CoverPath = covPath

	if Config.SaveAnimatedArtwork && meta.Data[0].Attributes.EditorialVideo.MotionSquare.Video != "" {
		fmt.Println("Found Animation Artwork.")

		motionvideoUrlSquare, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionSquare.Video)
		if err != nil {
			fmt.Println("no motion video square.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(playlistFolderPath, "square_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork square exists.")
			}
			if exists {
				fmt.Println("Animated artwork square already exists locally.")
			} else {
				fmt.Println("Animation Artwork Square Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlSquare, "-c", "copy", filepath.Join(playlistFolderPath, "square_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork square dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Square Downloaded")
				}
			}
		}

		if Config.EmbyAnimatedArtwork {
			cmd3 := exec.Command("ffmpeg", "-i", filepath.Join(playlistFolderPath, "square_animated_artwork.mp4"), "-vf", "scale=440:-1", "-r", "24", "-f", "gif", filepath.Join(playlistFolderPath, "folder.jpg"))
			if err := cmd3.Run(); err != nil {
				fmt.Printf("animated artwork square to gif err: %v\n", err)
			}
		}
	}
	if station.Type == "stream" {
		counter.Total++
		if isInArray(okDict[station.ID], 1) {
			counter.Success++
			return nil
		}
		songName := strings.NewReplacer(
			"{SongId}", station.ID,
			"{SongNumer}", "01",
			"{SongName}", LimitString(station.Name),
			"{ArtistName}", "Apple Music Station",
			"{DiscNumber}", "1",
			"{TrackNumber}", "1",
			"{Quality}", "256Kbps",
			"{Tag}", "",
			"{Codec}", "AAC",
		).Replace(Config.SongFileFormat)
		fmt.Println(songName)
		trackPath := filepath.Join(playlistFolderPath, fmt.Sprintf("%s.m4a", forbiddenNames.ReplaceAllString(songName, "_")))
		exists, _ := fileExists(trackPath)
		if exists {
			counter.Success++
			okDict[station.ID] = append(okDict[station.ID], 1)

			fmt.Println("Radio already exists locally.")
			return nil
		}
		assetsUrl, serverUrl, err := ampapi.GetStationAssetsUrlAndServerUrl(station.ID, mediaUserToken, token)
		if err != nil {
			fmt.Println("Failed to get station assets url.", err)
			counter.Error++
			return err
		}
		trackM3U8 := strings.ReplaceAll(assetsUrl, "index.m3u8", "256/prog_index.m3u8")
		keyAndUrls, _ := runv3.Run(station.ID, trackM3U8, token, mediaUserToken, true, serverUrl, nil)
		err = runv3.ExtMvData(keyAndUrls, trackPath)
		if err != nil {
			fmt.Println("Failed to download station stream.", err)
			counter.Error++
			return err
		}
		tags := []string{
			"tool=",
			"disk=1/1",
			"track=1",
			"tracknum=1/1",
			fmt.Sprintf("artist=%s", "Apple Music Station"),
			fmt.Sprintf("performer=%s", "Apple Music Station"),
			fmt.Sprintf("album_artist=%s", "Apple Music Station"),
			fmt.Sprintf("album=%s", station.Name),
			fmt.Sprintf("title=%s", station.Name),
		}
		if Config.EmbedCover {
			tags = append(tags, fmt.Sprintf("cover=%s", station.CoverPath))
		}
		tagsString := strings.Join(tags, ":")
		cmd := exec.Command("MP4Box", "-itags", tagsString, trackPath)
		if err := cmd.Run(); err != nil {
			fmt.Printf("Embed failed: %v\n", err)
		}
		counter.Success++
		okDict[station.ID] = append(okDict[station.ID], 1)
		return nil
	}

	for i := range station.Tracks {
		station.Tracks[i].CoverPath = covPath
		station.Tracks[i].SaveDir = playlistFolderPath
		station.Tracks[i].Codec = Codec
	}

	trackTotal := len(station.Tracks)
	arr := make([]int, trackTotal)
	for i := 0; i < trackTotal; i++ {
		arr[i] = i + 1
	}
	var selected []int

	if true {
		selected = arr
	}
	for i := range station.Tracks {
		i++
		if isInArray(selected, i) {
			ripTrack(&station.Tracks[i-1], token, mediaUserToken)
		}
	}
	return nil
}

func ripAlbum(albumId string, token string, storefront string, mediaUserToken string, urlArg_i string) error {
	album := task.NewAlbum(storefront, albumId)
	err := album.GetResp(token, Config.Language)
	if err != nil {
		fmt.Println("Failed to get album response.")
		return err
	}
	meta := album.Resp
	if debug_mode {
		fmt.Println(meta.Data[0].Attributes.ArtistName)
		fmt.Println(meta.Data[0].Attributes.Name)

		for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
			trackNum++
			fmt.Printf("\nTrack %d of %d:\n", trackNum, len(meta.Data[0].Relationships.Tracks.Data))
			fmt.Printf("%02d. %s\n", trackNum, track.Attributes.Name)

			manifest, err := ampapi.GetSongResp(storefront, track.ID, album.Language, token)
			if err != nil {
				fmt.Printf("Failed to get manifest for track %d: %v\n", trackNum, err)
				continue
			}

			var m3u8Url string
			if manifest.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls != "" {
				m3u8Url = manifest.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls
			}
			needCheck := false
			if Config.GetM3u8Mode == "all" {
				needCheck = true
			} else if Config.GetM3u8Mode == "hires" && contains(track.Attributes.AudioTraits, "hi-res-lossless") {
				needCheck = true
			}
			if needCheck {
				fullM3u8Url, err := checkM3u8(track.ID, "song")
				if err == nil && strings.HasSuffix(fullM3u8Url, ".m3u8") {
					m3u8Url = fullM3u8Url
				} else {
					fmt.Println("Failed to get best quality m3u8 from device m3u8 port, will use m3u8 from Web API")
				}
			}

			_, _, err = extractMedia(m3u8Url, true)
			if err != nil {
				fmt.Printf("Failed to extract quality info for track %d: %v\n", trackNum, err)
				continue
			}
		}
		return nil
	}
	var Codec string
	if dl_atmos {
		Codec = "ATMOS"
	} else if dl_aac {
		Codec = "AAC"
	} else {
		Codec = "ALAC"
	}
	album.Codec = Codec
	var singerFoldername string
	if Config.ArtistFolderFormat != "" {
		if len(meta.Data[0].Relationships.Artists.Data) > 0 {
			singerFoldername = strings.NewReplacer(
				"{UrlArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistId}", meta.Data[0].Relationships.Artists.Data[0].ID,
			).Replace(Config.ArtistFolderFormat)
		} else {
			singerFoldername = strings.NewReplacer(
				"{UrlArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
				"{ArtistId}", "",
			).Replace(Config.ArtistFolderFormat)
		}
		if strings.HasSuffix(singerFoldername, ".") {
			singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
		}
		singerFoldername = strings.TrimSpace(singerFoldername)
		fmt.Println(singerFoldername)
	}
	singerFolder := filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	if dl_atmos {
		singerFolder = filepath.Join(Config.AtmosSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	if dl_aac {
		singerFolder = filepath.Join(Config.AacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	os.MkdirAll(singerFolder, os.ModePerm)
	album.SaveDir = singerFolder
	var Quality string
	if strings.Contains(Config.AlbumFolderFormat, "Quality") {
		if dl_atmos {
			Quality = fmt.Sprintf("%dKbps", Config.AtmosMax-2000)
		} else if dl_aac && Config.AacType == "aac-lc" {
			Quality = "256Kbps"
		} else {
			manifest1, err := ampapi.GetSongResp(storefront, meta.Data[0].Relationships.Tracks.Data[0].ID, album.Language, token)
			if err != nil {
				fmt.Println("Failed to get manifest.\n", err)
			} else {
				if manifest1.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls == "" {
					Codec = "AAC"
					Quality = "256Kbps"
				} else {
					needCheck := false

					if Config.GetM3u8Mode == "all" {
						needCheck = true
					} else if Config.GetM3u8Mode == "hires" && contains(meta.Data[0].Relationships.Tracks.Data[0].Attributes.AudioTraits, "hi-res-lossless") {
						needCheck = true
					}
					var EnhancedHls_m3u8 string
					if needCheck {
						EnhancedHls_m3u8, _ = checkM3u8(meta.Data[0].Relationships.Tracks.Data[0].ID, "album")
						if strings.HasSuffix(EnhancedHls_m3u8, ".m3u8") {
							manifest1.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls = EnhancedHls_m3u8
						}
					}
					_, Quality, err = extractMedia(manifest1.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls, true)
					if err != nil {
						fmt.Println("Failed to extract quality from manifest.\n", err)
					}
				}
			}
		}
	}
	stringsToJoin := []string{}
	if meta.Data[0].Attributes.IsAppleDigitalMaster || meta.Data[0].Attributes.IsMasteredForItunes {
		if Config.AppleMasterChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.AppleMasterChoice)
		}
	}
	if meta.Data[0].Attributes.ContentRating == "explicit" {
		if Config.ExplicitChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.ExplicitChoice)
		}
	}
	if meta.Data[0].Attributes.ContentRating == "clean" {
		if Config.CleanChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.CleanChoice)
		}
	}
	Tag_string := strings.Join(stringsToJoin, " ")
	var albumFolderName string
	albumFolderName = strings.NewReplacer(
		"{ReleaseDate}", meta.Data[0].Attributes.ReleaseDate,
		"{ReleaseYear}", meta.Data[0].Attributes.ReleaseDate[:4],
		"{ArtistName}", LimitString(meta.Data[0].Attributes.ArtistName),
		"{AlbumName}", LimitString(meta.Data[0].Attributes.Name),
		"{UPC}", meta.Data[0].Attributes.Upc,
		"{RecordLabel}", meta.Data[0].Attributes.RecordLabel,
		"{Copyright}", meta.Data[0].Attributes.Copyright,
		"{AlbumId}", albumId,
		"{Quality}", Quality,
		"{Codec}", Codec,
		"{Tag}", Tag_string,
	).Replace(Config.AlbumFolderFormat)

	if strings.HasSuffix(albumFolderName, ".") {
		albumFolderName = strings.ReplaceAll(albumFolderName, ".", "")
	}
	albumFolderName = strings.TrimSpace(albumFolderName)
	albumFolderPath := filepath.Join(singerFolder, forbiddenNames.ReplaceAllString(albumFolderName, "_"))
	os.MkdirAll(albumFolderPath, os.ModePerm)
	album.SaveName = albumFolderName
	fmt.Println(albumFolderName)
	if Config.SaveArtistCover && len(meta.Data[0].Relationships.Artists.Data) > 0{
		if meta.Data[0].Relationships.Artists.Data[0].Attributes.Artwork.Url != "" {
			_, err = writeCover(singerFolder, "folder", meta.Data[0].Relationships.Artists.Data[0].Attributes.Artwork.Url)
			if err != nil {
				fmt.Println("Failed to write artist cover.")
			}
		}
	}
	covPath, err := writeCover(albumFolderPath, "cover", meta.Data[0].Attributes.Artwork.URL)
	if err != nil {
		fmt.Println("Failed to write cover.")
	}
	if Config.SaveAnimatedArtwork && meta.Data[0].Attributes.EditorialVideo.MotionDetailSquare.Video != "" {
		fmt.Println("Found Animation Artwork.")

		motionvideoUrlSquare, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionDetailSquare.Video)
		if err != nil {
			fmt.Println("no motion video square.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(albumFolderPath, "square_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork square exists.")
			}
			if exists {
				fmt.Println("Animated artwork square already exists locally.")
			} else {
				fmt.Println("Animation Artwork Square Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlSquare, "-c", "copy", filepath.Join(albumFolderPath, "square_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork square dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Square Downloaded")
				}
			}
		}

		if Config.EmbyAnimatedArtwork {
			cmd3 := exec.Command("ffmpeg", "-i", filepath.Join(albumFolderPath, "square_animated_artwork.mp4"), "-vf", "scale=440:-1", "-r", "24", "-f", "gif", filepath.Join(albumFolderPath, "folder.jpg"))
			if err := cmd3.Run(); err != nil {
				fmt.Printf("animated artwork square to gif err: %v\n", err)
			}
		}

		motionvideoUrlTall, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionDetailTall.Video)
		if err != nil {
			fmt.Println("no motion video tall.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(albumFolderPath, "tall_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork tall exists.")
			}
			if exists {
				fmt.Println("Animated artwork tall already exists locally.")
			} else {
				fmt.Println("Animation Artwork Tall Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlTall, "-c", "copy", filepath.Join(albumFolderPath, "tall_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork tall dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Tall Downloaded")
				}
			}
		}
	}
	for i := range album.Tracks {
		album.Tracks[i].CoverPath = covPath
		album.Tracks[i].SaveDir = albumFolderPath
		album.Tracks[i].Codec = Codec
	}
	trackTotal := len(meta.Data[0].Relationships.Tracks.Data)
	arr := make([]int, trackTotal)
	for i := 0; i < trackTotal; i++ {
		arr[i] = i + 1
	}

	if dl_song {
		if urlArg_i == "" {
		} else {
			for i := range album.Tracks {
				if urlArg_i == album.Tracks[i].ID {
					ripTrack(&album.Tracks[i], token, mediaUserToken)
					return nil
				}
			}
		}
		return nil
	}
	var selected []int
	if !dl_select {
		selected = arr
	} else {
		selected = album.ShowSelect()
	}
	for i := range album.Tracks {
		i++
		if isInArray(okDict[albumId], i) {
			counter.Total++
			counter.Success++
			continue
		}
		if isInArray(selected, i) {
			ripTrack(&album.Tracks[i-1], token, mediaUserToken)
		}
	}
	return nil

}
func ripPlaylist(playlistId string, token string, storefront string, mediaUserToken string) error {
	playlist := task.NewPlaylist(storefront, playlistId)
	err := playlist.GetResp(token, Config.Language)
	if err != nil {
		fmt.Println("Failed to get playlist response.")
		return err
	}
	meta := playlist.Resp
	if debug_mode {
		fmt.Println(meta.Data[0].Attributes.ArtistName)
		fmt.Println(meta.Data[0].Attributes.Name)

		for trackNum, track := range meta.Data[0].Relationships.Tracks.Data {
			trackNum++
			fmt.Printf("\nTrack %d of %d:\n", trackNum, len(meta.Data[0].Relationships.Tracks.Data))
			fmt.Printf("%02d. %s\n", trackNum, track.Attributes.Name)

			manifest, err := ampapi.GetSongResp(storefront, track.ID, playlist.Language, token)
			if err != nil {
				fmt.Printf("Failed to get manifest for track %d: %v\n", trackNum, err)
				continue
			}

			var m3u8Url string
			if manifest.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls != "" {
				m3u8Url = manifest.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls
			}
			needCheck := false
			if Config.GetM3u8Mode == "all" {
				needCheck = true
			} else if Config.GetM3u8Mode == "hires" && contains(track.Attributes.AudioTraits, "hi-res-lossless") {
				needCheck = true
			}
			if needCheck {
				fullM3u8Url, err := checkM3u8(track.ID, "song")
				if err == nil && strings.HasSuffix(fullM3u8Url, ".m3u8") {
					m3u8Url = fullM3u8Url
				} else {
					fmt.Println("Failed to get best quality m3u8 from device m3u8 port, will use m3u8 from Web API")
				}
			}

			_, _, err = extractMedia(m3u8Url, true)
			if err != nil {
				fmt.Printf("Failed to extract quality info for track %d: %v\n", trackNum, err)
				continue
			}
		}
		return nil
	}
	var Codec string
	if dl_atmos {
		Codec = "ATMOS"
	} else if dl_aac {
		Codec = "AAC"
	} else {
		Codec = "ALAC"
	}
	playlist.Codec = Codec
	var singerFoldername string
	if Config.ArtistFolderFormat != "" {
		singerFoldername = strings.NewReplacer(
			"{ArtistName}", "Apple Music",
			"{ArtistId}", "",
			"{UrlArtistName}", "Apple Music",
		).Replace(Config.ArtistFolderFormat)
		if strings.HasSuffix(singerFoldername, ".") {
			singerFoldername = strings.ReplaceAll(singerFoldername, ".", "")
		}
		singerFoldername = strings.TrimSpace(singerFoldername)
		fmt.Println(singerFoldername)
	}
	singerFolder := filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	if dl_atmos {
		singerFolder = filepath.Join(Config.AtmosSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	if dl_aac {
		singerFolder = filepath.Join(Config.AacSaveFolder, forbiddenNames.ReplaceAllString(singerFoldername, "_"))
	}
	os.MkdirAll(singerFolder, os.ModePerm)
	playlist.SaveDir = singerFolder

	var Quality string
	if strings.Contains(Config.AlbumFolderFormat, "Quality") {
		if dl_atmos {
			Quality = fmt.Sprintf("%dKbps", Config.AtmosMax-2000)
		} else if dl_aac && Config.AacType == "aac-lc" {
			Quality = "256Kbps"
		} else {
			manifest1, err := ampapi.GetSongResp(storefront, meta.Data[0].Relationships.Tracks.Data[0].ID, playlist.Language, token)
			if err != nil {
				fmt.Println("Failed to get manifest.\n", err)
			} else {
				if manifest1.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls == "" {
					Codec = "AAC"
					Quality = "256Kbps"
				} else {
					needCheck := false

					if Config.GetM3u8Mode == "all" {
						needCheck = true
					} else if Config.GetM3u8Mode == "hires" && contains(meta.Data[0].Relationships.Tracks.Data[0].Attributes.AudioTraits, "hi-res-lossless") {
						needCheck = true
					}
					var EnhancedHls_m3u8 string
					if needCheck {
						EnhancedHls_m3u8, _ = checkM3u8(meta.Data[0].Relationships.Tracks.Data[0].ID, "album")
						if strings.HasSuffix(EnhancedHls_m3u8, ".m3u8") {
							manifest1.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls = EnhancedHls_m3u8
						}
					}
					_, Quality, err = extractMedia(manifest1.Data[0].Attributes.ExtendedAssetUrls.EnhancedHls, true)
					if err != nil {
						fmt.Println("Failed to extract quality from manifest.\n", err)
					}
				}
			}
		}
	}
	stringsToJoin := []string{}
	if meta.Data[0].Attributes.IsAppleDigitalMaster || meta.Data[0].Attributes.IsMasteredForItunes {
		if Config.AppleMasterChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.AppleMasterChoice)
		}
	}
	if meta.Data[0].Attributes.ContentRating == "explicit" {
		if Config.ExplicitChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.ExplicitChoice)
		}
	}
	if meta.Data[0].Attributes.ContentRating == "clean" {
		if Config.CleanChoice != "" {
			stringsToJoin = append(stringsToJoin, Config.CleanChoice)
		}
	}
	Tag_string := strings.Join(stringsToJoin, " ")
	playlistFolder := strings.NewReplacer(
		"{ArtistName}", "Apple Music",
		"{PlaylistName}", LimitString(meta.Data[0].Attributes.Name),
		"{PlaylistId}", playlistId,
		"{Quality}", Quality,
		"{Codec}", Codec,
		"{Tag}", Tag_string,
	).Replace(Config.PlaylistFolderFormat)
	if strings.HasSuffix(playlistFolder, ".") {
		playlistFolder = strings.ReplaceAll(playlistFolder, ".", "")
	}
	playlistFolder = strings.TrimSpace(playlistFolder)
	playlistFolderPath := filepath.Join(singerFolder, forbiddenNames.ReplaceAllString(playlistFolder, "_"))
	os.MkdirAll(playlistFolderPath, os.ModePerm)
	playlist.SaveName = playlistFolder
	fmt.Println(playlistFolder)
	covPath, err := writeCover(playlistFolderPath, "cover", meta.Data[0].Attributes.Artwork.URL)
	if err != nil {
		fmt.Println("Failed to write cover.")
	}

	for i := range playlist.Tracks {
		playlist.Tracks[i].CoverPath = covPath
		playlist.Tracks[i].SaveDir = playlistFolderPath
		playlist.Tracks[i].Codec = Codec
	}

	if Config.SaveAnimatedArtwork && meta.Data[0].Attributes.EditorialVideo.MotionDetailSquare.Video != "" {
		fmt.Println("Found Animation Artwork.")

		motionvideoUrlSquare, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionDetailSquare.Video)
		if err != nil {
			fmt.Println("no motion video square.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(playlistFolderPath, "square_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork square exists.")
			}
			if exists {
				fmt.Println("Animated artwork square already exists locally.")
			} else {
				fmt.Println("Animation Artwork Square Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlSquare, "-c", "copy", filepath.Join(playlistFolderPath, "square_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork square dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Square Downloaded")
				}
			}
		}

		if Config.EmbyAnimatedArtwork {
			cmd3 := exec.Command("ffmpeg", "-i", filepath.Join(playlistFolderPath, "square_animated_artwork.mp4"), "-vf", "scale=440:-1", "-r", "24", "-f", "gif", filepath.Join(playlistFolderPath, "folder.jpg"))
			if err := cmd3.Run(); err != nil {
				fmt.Printf("animated artwork square to gif err: %v\n", err)
			}
		}

		motionvideoUrlTall, err := extractVideo(meta.Data[0].Attributes.EditorialVideo.MotionDetailTall.Video)
		if err != nil {
			fmt.Println("no motion video tall.\n", err)
		} else {
			exists, err := fileExists(filepath.Join(playlistFolderPath, "tall_animated_artwork.mp4"))
			if err != nil {
				fmt.Println("Failed to check if animated artwork tall exists.")
			}
			if exists {
				fmt.Println("Animated artwork tall already exists locally.")
			} else {
				fmt.Println("Animation Artwork Tall Downloading...")
				cmd := exec.Command("ffmpeg", "-loglevel", "quiet", "-y", "-i", motionvideoUrlTall, "-c", "copy", filepath.Join(playlistFolderPath, "tall_animated_artwork.mp4"))
				if err := cmd.Run(); err != nil {
					fmt.Printf("animated artwork tall dl err: %v\n", err)
				} else {
					fmt.Println("Animation Artwork Tall Downloaded")
				}
			}
		}
	}
	trackTotal := len(meta.Data[0].Relationships.Tracks.Data)
	arr := make([]int, trackTotal)
	for i := 0; i < trackTotal; i++ {
		arr[i] = i + 1
	}
	var selected []int

	if !dl_select {
		selected = arr
	} else {
		selected = playlist.ShowSelect()
	}
	for i := range playlist.Tracks {
		i++
		if isInArray(okDict[playlistId], i) {
			counter.Total++
			counter.Success++
			continue
		}
		if isInArray(selected, i) {
			ripTrack(&playlist.Tracks[i-1], token, mediaUserToken)
		}
	}
	return nil
}

func writeMP4Tags(track *task.Track, lrc string) error {
	t := &mp4tag.MP4Tags{
		Title:      track.Resp.Attributes.Name,
		TitleSort:  track.Resp.Attributes.Name,
		Artist:     track.Resp.Attributes.ArtistName,
		ArtistSort: track.Resp.Attributes.ArtistName,
		Custom: map[string]string{
			"PERFORMER":   track.Resp.Attributes.ArtistName,
			"RELEASETIME": track.Resp.Attributes.ReleaseDate,
			"ISRC":        track.Resp.Attributes.Isrc,
			"LABEL":       "",
			"UPC":         "",
		},
		Composer:     track.Resp.Attributes.ComposerName,
		ComposerSort: track.Resp.Attributes.ComposerName,
		CustomGenre:  track.Resp.Attributes.GenreNames[0],
		Lyrics:       lrc,
		TrackNumber:  int16(track.Resp.Attributes.TrackNumber),
		DiscNumber:   int16(track.Resp.Attributes.DiscNumber),
		Album:        track.Resp.Attributes.AlbumName,
		AlbumSort:    track.Resp.Attributes.AlbumName,
	}

	if track.PreType == "albums" {
		albumID, err := strconv.ParseUint(track.PreID, 10, 32)
		if err != nil {
			return err
		}
		t.ItunesAlbumID = int32(albumID)
	}

	if len(track.Resp.Relationships.Artists.Data) > 0 {
		artistID, err := strconv.ParseUint(track.Resp.Relationships.Artists.Data[0].ID, 10, 32)
		if err != nil {
			return err
		}
		t.ItunesArtistID = int32(artistID)
	}

	if (track.PreType == "playlists" || track.PreType == "stations") && !Config.UseSongInfoForPlaylist {
		t.DiscNumber = 1
		t.DiscTotal = 1
		t.TrackNumber = int16(track.TaskNum)
		t.TrackTotal = int16(track.TaskTotal)
		t.Album = track.PlaylistData.Attributes.Name
		t.AlbumSort = track.PlaylistData.Attributes.Name
		t.AlbumArtist = track.PlaylistData.Attributes.ArtistName
		t.AlbumArtistSort = track.PlaylistData.Attributes.ArtistName
	} else if (track.PreType == "playlists" || track.PreType == "stations") && Config.UseSongInfoForPlaylist {
		t.DiscTotal = int16(track.DiscTotal)
		t.TrackTotal = int16(track.AlbumData.Attributes.TrackCount)
		t.AlbumArtist = track.AlbumData.Attributes.ArtistName
		t.AlbumArtistSort = track.AlbumData.Attributes.ArtistName
		t.Custom["UPC"] = track.AlbumData.Attributes.Upc
		t.Custom["LABEL"] = track.AlbumData.Attributes.RecordLabel
		t.Date = track.AlbumData.Attributes.ReleaseDate
		t.Copyright = track.AlbumData.Attributes.Copyright
		t.Publisher = track.AlbumData.Attributes.RecordLabel
	} else {
		t.DiscTotal = int16(track.DiscTotal)
		t.TrackTotal = int16(track.AlbumData.Attributes.TrackCount)
		t.AlbumArtist = track.AlbumData.Attributes.ArtistName
		t.AlbumArtistSort = track.AlbumData.Attributes.ArtistName
		t.Custom["UPC"] = track.AlbumData.Attributes.Upc
		t.Date = track.AlbumData.Attributes.ReleaseDate
		t.Copyright = track.AlbumData.Attributes.Copyright
		t.Publisher = track.AlbumData.Attributes.RecordLabel
	}

	if track.Resp.Attributes.ContentRating == "explicit" {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryExplicit
	} else if track.Resp.Attributes.ContentRating == "clean" {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryClean
	} else {
		t.ItunesAdvisory = mp4tag.ItunesAdvisoryNone
	}

	mp4, err := mp4tag.Open(track.SavePath)
	if err != nil {
		return err
	}
	defer mp4.Close()
	err = mp4.Write(t, []string{})
	if err != nil {
		return err
	}
	return nil
}

func main() {
	err := loadConfig()
	if err != nil {
		fmt.Printf("load Config failed: %v", err)
		return
	}
	token, err := ampapi.GetToken()
	if err != nil {
		if Config.AuthorizationToken != "" && Config.AuthorizationToken != "your-authorization-token" {
			token = strings.Replace(Config.AuthorizationToken, "Bearer ", "", -1)
		} else {
			fmt.Println("Failed to get token.")
			return
		}
	}
	var search_type string
	var bot_mode bool
	pflag.StringVar(&search_type, "search", "", "Search for 'album', 'song', or 'artist'. Provide query after flags.")
	pflag.BoolVar(&bot_mode, "bot", false, "Run Telegram bot mode")
	pflag.BoolVar(&dl_atmos, "atmos", false, "Enable atmos download mode")
	pflag.BoolVar(&dl_aac, "aac", false, "Enable adm-aac download mode")
	pflag.BoolVar(&dl_select, "select", false, "Enable selective download")
	pflag.BoolVar(&dl_song, "song", false, "Enable single song download mode")
	pflag.BoolVar(&artist_select, "all-album", false, "Download all artist albums")
	pflag.BoolVar(&debug_mode, "debug", false, "Enable debug mode to show audio quality information")
	alac_max = pflag.Int("alac-max", Config.AlacMax, "Specify the max quality for download alac")
	atmos_max = pflag.Int("atmos-max", Config.AtmosMax, "Specify the max quality for download atmos")
	aac_type = pflag.String("aac-type", Config.AacType, "Select AAC type, aac aac-binaural aac-downmix")
	mv_audio_type = pflag.String("mv-audio-type", Config.MVAudioType, "Select MV audio type, atmos ac3 aac")
	mv_max = pflag.Int("mv-max", Config.MVMax, "Specify the max quality for download MV")

	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [url1 url2 ...]\n", "[main | main.exe | go run main.go]")
		fmt.Fprintf(os.Stderr, "Search Usage: %s --search [album|song|artist] [query]\n", "[main | main.exe | go run main.go]")
		fmt.Println("\nOptions:")
		pflag.PrintDefaults()
	}

	pflag.Parse()
	Config.AlacMax = *alac_max
	Config.AtmosMax = *atmos_max
	Config.AacType = *aac_type
	Config.MVAudioType = *mv_audio_type
	Config.MVMax = *mv_max

	if bot_mode {
		runTelegramBot(token)
		return
	}

	args := pflag.Args()

	if search_type != "" {
		if len(args) == 0 {
			fmt.Println("Error: --search flag requires a query.")
			pflag.Usage()
			return
		}
		selectedUrl, err := handleSearch(search_type, args, token)
		if err != nil {
			fmt.Printf("\nSearch process failed: %v\n", err)
			return
		}
		if selectedUrl == "" {
			fmt.Println("\nExiting.")
			return
		}
		os.Args = []string{selectedUrl}
	} else {
		if len(args) == 0 {
			fmt.Println("No URLs provided. Please provide at least one URL.")
			pflag.Usage()
			return
		}
		os.Args = args
	}

	if strings.Contains(os.Args[0], "/artist/") {
		urlArtistName, urlArtistID, err := getUrlArtistName(os.Args[0], token)
		if err != nil {
			fmt.Println("Failed to get artistname.")
			return
		}
		Config.ArtistFolderFormat = strings.NewReplacer(
			"{UrlArtistName}", LimitString(urlArtistName),
			"{ArtistId}", urlArtistID,
		).Replace(Config.ArtistFolderFormat)
		albumArgs, err := checkArtist(os.Args[0], token, "albums")
		if err != nil {
			fmt.Println("Failed to get artist albums.")
			return
		}
		mvArgs, err := checkArtist(os.Args[0], token, "music-videos")
		if err != nil {
			fmt.Println("Failed to get artist music-videos.")
		}
		os.Args = append(albumArgs, mvArgs...)
	}
	albumTotal := len(os.Args)
	for {
		for albumNum, urlRaw := range os.Args {
			fmt.Printf("Queue %d of %d: ", albumNum+1, albumTotal)
			var storefront, albumId string

			if strings.Contains(urlRaw, "/music-video/") {
				fmt.Println("Music Video")
				if debug_mode {
					continue
				}
				counter.Total++
				if len(Config.MediaUserToken) <= 50 {
					fmt.Println(": meida-user-token is not set, skip MV dl")
					counter.Success++
					continue
				}
				if _, err := exec.LookPath("mp4decrypt"); err != nil {
					fmt.Println(": mp4decrypt is not found, skip MV dl")
					counter.Success++
					continue
				}
				mvSaveDir := strings.NewReplacer(
					"{ArtistName}", "",
					"{UrlArtistName}", "",
					"{ArtistId}", "",
				).Replace(Config.ArtistFolderFormat)
				if mvSaveDir != "" {
					mvSaveDir = filepath.Join(Config.AlacSaveFolder, forbiddenNames.ReplaceAllString(mvSaveDir, "_"))
				} else {
					mvSaveDir = Config.AlacSaveFolder
				}
				storefront, albumId = checkUrlMv(urlRaw)
				err := mvDownloader(albumId, mvSaveDir, token, storefront, Config.MediaUserToken, nil)
				if err != nil {
					fmt.Println("\u26A0 Failed to dl MV:", err)
					counter.Error++
					continue
				}
				counter.Success++
				continue
			}
			if strings.Contains(urlRaw, "/song/") {
				fmt.Printf("Song->")
				storefront, songId := checkUrlSong(urlRaw)
				if storefront == "" || songId == "" {
					fmt.Println("Invalid song URL format.")
					continue
				}
				err := ripSong(songId, token, storefront, Config.MediaUserToken)
				if err != nil {
					fmt.Println("Failed to rip song:", err)
				}
				continue
			}
			parse, err := url.Parse(urlRaw)
			if err != nil {
				log.Fatalf("Invalid URL: %v", err)
			}
			var urlArg_i = parse.Query().Get("i")

			if strings.Contains(urlRaw, "/album/") {
				fmt.Println("Album")
				storefront, albumId = checkUrl(urlRaw)
				err := ripAlbum(albumId, token, storefront, Config.MediaUserToken, urlArg_i)
				if err != nil {
					fmt.Println("Failed to rip album:", err)
				}
			} else if strings.Contains(urlRaw, "/playlist/") {
				fmt.Println("Playlist")
				storefront, albumId = checkUrlPlaylist(urlRaw)
				err := ripPlaylist(albumId, token, storefront, Config.MediaUserToken)
				if err != nil {
					fmt.Println("Failed to rip playlist:", err)
				}
			} else if strings.Contains(urlRaw, "/station/") {
				fmt.Printf("Station")
				storefront, albumId = checkUrlStation(urlRaw)
				if len(Config.MediaUserToken) <= 50 {
					fmt.Println(": meida-user-token is not set, skip station dl")
					continue
				}
				err := ripStation(albumId, token, storefront, Config.MediaUserToken)
				if err != nil {
					fmt.Println("Failed to rip station:", err)
				}
			} else {
				fmt.Println("Invalid type")
			}
		}
		fmt.Printf("=======  [\u2714 ] Completed: %d/%d  |  [\u26A0 ] Warnings: %d  |  [\u2716 ] Errors: %d  =======\n", counter.Success, counter.Total, counter.Unavailable+counter.NotSong, counter.Error)
		if counter.Error == 0 {
			break
		}
		fmt.Println("Error detected, press Enter to try again...")
		fmt.Scanln()
		fmt.Println("Start trying again...")
		counter = structs.Counter{}
	}
}

func mvDownloader(adamID string, saveDir string, token string, storefront string, mediaUserToken string, track *task.Track) error {
	MVInfo, err := ampapi.GetMusicVideoResp(storefront, adamID, Config.Language, token)
	if err != nil {
		fmt.Println("\u26A0 Failed to get MV manifest:", err)
		return nil
	}

	if strings.HasSuffix(saveDir, ".") {
		saveDir = strings.ReplaceAll(saveDir, ".", "")
	}
	saveDir = strings.TrimSpace(saveDir)

	vidPath := filepath.Join(saveDir, fmt.Sprintf("%s_vid.mp4", adamID))
	audPath := filepath.Join(saveDir, fmt.Sprintf("%s_aud.mp4", adamID))
	mvSaveName := fmt.Sprintf("%s (%s)", MVInfo.Data[0].Attributes.Name, adamID)
	if track != nil {
		mvSaveName = fmt.Sprintf("%02d. %s", track.TaskNum, MVInfo.Data[0].Attributes.Name)
	}

	mvOutPath := filepath.Join(saveDir, fmt.Sprintf("%s.mp4", forbiddenNames.ReplaceAllString(mvSaveName, "_")))

	fmt.Println(MVInfo.Data[0].Attributes.Name)

	exists, _ := fileExists(mvOutPath)
	if exists {
		fmt.Println("MV already exists locally.")
		return nil
	}

	mvm3u8url, _, _, _ := runv3.GetWebplayback(adamID, token, mediaUserToken, true)
	if mvm3u8url == "" {
		return errors.New("media-user-token may wrong or expired")
	}

	os.MkdirAll(saveDir, os.ModePerm)
	videom3u8url, _ := extractVideo(mvm3u8url)
	videokeyAndUrls, _ := runv3.Run(adamID, videom3u8url, token, mediaUserToken, true, "", nil)
	_ = runv3.ExtMvData(videokeyAndUrls, vidPath)
	defer os.Remove(vidPath)
	audiom3u8url, _ := extractMvAudio(mvm3u8url)
	audiokeyAndUrls, _ := runv3.Run(adamID, audiom3u8url, token, mediaUserToken, true, "", nil)
	_ = runv3.ExtMvData(audiokeyAndUrls, audPath)
	defer os.Remove(audPath)

	tags := []string{
		"tool=",
		fmt.Sprintf("artist=%s", MVInfo.Data[0].Attributes.ArtistName),
		fmt.Sprintf("title=%s", MVInfo.Data[0].Attributes.Name),
		fmt.Sprintf("genre=%s", MVInfo.Data[0].Attributes.GenreNames[0]),
		fmt.Sprintf("created=%s", MVInfo.Data[0].Attributes.ReleaseDate),
		fmt.Sprintf("ISRC=%s", MVInfo.Data[0].Attributes.Isrc),
	}

	if MVInfo.Data[0].Attributes.ContentRating == "explicit" {
		tags = append(tags, "rating=1")
	} else if MVInfo.Data[0].Attributes.ContentRating == "clean" {
		tags = append(tags, "rating=2")
	} else {
		tags = append(tags, "rating=0")
	}

	if track != nil {
		if track.PreType == "playlists" && !Config.UseSongInfoForPlaylist {
			tags = append(tags, "disk=1/1")
			tags = append(tags, fmt.Sprintf("album=%s", track.PlaylistData.Attributes.Name))
			tags = append(tags, fmt.Sprintf("track=%d", track.TaskNum))
			tags = append(tags, fmt.Sprintf("tracknum=%d/%d", track.TaskNum, track.TaskTotal))
			tags = append(tags, fmt.Sprintf("album_artist=%s", track.PlaylistData.Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("performer=%s", track.Resp.Attributes.ArtistName))
		} else if track.PreType == "playlists" && Config.UseSongInfoForPlaylist {
			tags = append(tags, fmt.Sprintf("album=%s", track.AlbumData.Attributes.Name))
			tags = append(tags, fmt.Sprintf("disk=%d/%d", track.Resp.Attributes.DiscNumber, track.DiscTotal))
			tags = append(tags, fmt.Sprintf("track=%d", track.Resp.Attributes.TrackNumber))
			tags = append(tags, fmt.Sprintf("tracknum=%d/%d", track.Resp.Attributes.TrackNumber, track.AlbumData.Attributes.TrackCount))
			tags = append(tags, fmt.Sprintf("album_artist=%s", track.AlbumData.Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("performer=%s", track.Resp.Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("copyright=%s", track.AlbumData.Attributes.Copyright))
			tags = append(tags, fmt.Sprintf("UPC=%s", track.AlbumData.Attributes.Upc))
		} else {
			tags = append(tags, fmt.Sprintf("album=%s", track.AlbumData.Attributes.Name))
			tags = append(tags, fmt.Sprintf("disk=%d/%d", track.Resp.Attributes.DiscNumber, track.DiscTotal))
			tags = append(tags, fmt.Sprintf("track=%d", track.Resp.Attributes.TrackNumber))
			tags = append(tags, fmt.Sprintf("tracknum=%d/%d", track.Resp.Attributes.TrackNumber, track.AlbumData.Attributes.TrackCount))
			tags = append(tags, fmt.Sprintf("album_artist=%s", track.AlbumData.Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("performer=%s", track.Resp.Attributes.ArtistName))
			tags = append(tags, fmt.Sprintf("copyright=%s", track.AlbumData.Attributes.Copyright))
			tags = append(tags, fmt.Sprintf("UPC=%s", track.AlbumData.Attributes.Upc))
		}
	} else {
		tags = append(tags, fmt.Sprintf("album=%s", MVInfo.Data[0].Attributes.AlbumName))
		tags = append(tags, fmt.Sprintf("disk=%d", MVInfo.Data[0].Attributes.DiscNumber))
		tags = append(tags, fmt.Sprintf("track=%d", MVInfo.Data[0].Attributes.TrackNumber))
		tags = append(tags, fmt.Sprintf("tracknum=%d", MVInfo.Data[0].Attributes.TrackNumber))
		tags = append(tags, fmt.Sprintf("performer=%s", MVInfo.Data[0].Attributes.ArtistName))
	}

	var covPath string
	if true {
		thumbURL := MVInfo.Data[0].Attributes.Artwork.URL
		baseThumbName := forbiddenNames.ReplaceAllString(mvSaveName, "_") + "_thumbnail"
		covPath, err = writeCover(saveDir, baseThumbName, thumbURL)
		if err != nil {
			fmt.Println("Failed to save MV thumbnail:", err)
		} else {
			tags = append(tags, fmt.Sprintf("cover=%s", covPath))
		}
	}
	defer os.Remove(covPath)

	tagsString := strings.Join(tags, ":")
	muxCmd := exec.Command("MP4Box", "-itags", tagsString, "-quiet", "-add", vidPath, "-add", audPath, "-keep-utc", "-new", mvOutPath)
	fmt.Printf("MV Remuxing...")
	if err := muxCmd.Run(); err != nil {
		fmt.Printf("MV mux failed: %v\n", err)
		return err
	}
	fmt.Printf("\rMV Remuxed.   \n")
	return nil
}

func extractMvAudio(c string) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}

	resp, err := http.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	audioString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(audioString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}

	audio := from.(*m3u8.MasterPlaylist)

	var audioPriority = []string{"audio-atmos", "audio-ac3", "audio-stereo-256"}
	if Config.MVAudioType == "ac3" {
		audioPriority = []string{"audio-ac3", "audio-stereo-256"}
	} else if Config.MVAudioType == "aac" {
		audioPriority = []string{"audio-stereo-256"}
	}

	re := regexp.MustCompile(`_gr(\d+)_`)

	type AudioStream struct {
		URL     string
		Rank    int
		GroupID string
	}
	var audioStreams []AudioStream

	for _, variant := range audio.Variants {
		for _, audiov := range variant.Alternatives {
			if audiov.URI != "" {
				for _, priority := range audioPriority {
					if audiov.GroupId == priority {
						matches := re.FindStringSubmatch(audiov.URI)
						if len(matches) == 2 {
							var rank int
							fmt.Sscanf(matches[1], "%d", &rank)
							streamUrl, _ := MediaUrl.Parse(audiov.URI)
							audioStreams = append(audioStreams, AudioStream{
								URL:     streamUrl.String(),
								Rank:    rank,
								GroupID: audiov.GroupId,
							})
						}
					}
				}
			}
		}
	}

	if len(audioStreams) == 0 {
		return "", errors.New("no suitable audio stream found")
	}

	sort.Slice(audioStreams, func(i, j int) bool {
		return audioStreams[i].Rank > audioStreams[j].Rank
	})
	fmt.Println("Audio: " + audioStreams[0].GroupID)
	return audioStreams[0].URL, nil
}

func checkM3u8(b string, f string) (string, error) {
	var EnhancedHls string
	if Config.GetM3u8FromDevice {
		adamID := b
		conn, err := net.Dial("tcp", Config.GetM3u8Port)
		if err != nil {
			fmt.Println("Error connecting to device:", err)
			return "none", err
		}
		defer conn.Close()
		if f == "song" {
			fmt.Println("Connected to device")
		}

		adamIDBuffer := []byte(adamID)
		lengthBuffer := []byte{byte(len(adamIDBuffer))}

		_, err = conn.Write(lengthBuffer)
		if err != nil {
			fmt.Println("Error writing length to device:", err)
			return "none", err
		}

		_, err = conn.Write(adamIDBuffer)
		if err != nil {
			fmt.Println("Error writing adamID to device:", err)
			return "none", err
		}

		response, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			fmt.Println("Error reading response from device:", err)
			return "none", err
		}

		response = bytes.TrimSpace(response)
		if len(response) > 0 {
			if f == "song" {
				fmt.Println("Received URL:", string(response))
			}
			EnhancedHls = string(response)
		} else {
			fmt.Println("Received an empty response")
		}
	}
	return EnhancedHls, nil
}

func formatAvailability(available bool, quality string) string {
	if !available {
		return "Not Available"
	}
	return quality
}

func extractMedia(b string, more_mode bool) (string, string, error) {
	masterUrl, err := url.Parse(b)
	if err != nil {
		return "", "", err
	}
	resp, err := http.Get(b)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	masterString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(masterString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", "", errors.New("m3u8 not of master type")
	}
	master := from.(*m3u8.MasterPlaylist)
	var streamUrl *url.URL
	sort.Slice(master.Variants, func(i, j int) bool {
		return master.Variants[i].AverageBandwidth > master.Variants[j].AverageBandwidth
	})
	if debug_mode && more_mode {
		fmt.Println("\nDebug: All Available Variants:")
		var data [][]string
		for _, variant := range master.Variants {
			data = append(data, []string{variant.Codecs, variant.Audio, fmt.Sprint(variant.Bandwidth)})
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Codec", "Audio", "Bandwidth"})
		table.SetAutoMergeCells(true)
		table.SetRowLine(true)
		table.AppendBulk(data)
		table.Render()

		var hasAAC, hasLossless, hasHiRes, hasAtmos, hasDolbyAudio bool
		var aacQuality, losslessQuality, hiResQuality, atmosQuality, dolbyAudioQuality string

		for _, variant := range master.Variants {
			if variant.Codecs == "mp4a.40.2" { // AAC
				hasAAC = true
				split := strings.Split(variant.Audio, "-")
				if len(split) >= 3 {
					bitrate, _ := strconv.Atoi(split[2])
					currentBitrate := 0
					if aacQuality != "" {
						current := strings.Split(aacQuality, " | ")[2]
						current = strings.Split(current, " ")[0]
						currentBitrate, _ = strconv.Atoi(current)
					}
					if bitrate > currentBitrate {
						aacQuality = fmt.Sprintf("AAC | 2 Channel | %d Kbps", bitrate)
					}
				}
			} else if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") { // Dolby Atmos
				hasAtmos = true
				split := strings.Split(variant.Audio, "-")
				if len(split) > 0 {
					bitrateStr := split[len(split)-1]
					if len(bitrateStr) == 4 && bitrateStr[0] == '2' {
						bitrateStr = bitrateStr[1:]
					}
					bitrate, _ := strconv.Atoi(bitrateStr)
					currentBitrate := 0
					if atmosQuality != "" {
						current := strings.Split(strings.Split(atmosQuality, " | ")[2], " ")[0]
						currentBitrate, _ = strconv.Atoi(current)
					}
					if bitrate > currentBitrate {
						atmosQuality = fmt.Sprintf("E-AC-3 | 16 Channel | %d Kbps", bitrate)
					}
				}
			} else if variant.Codecs == "alac" { // ALAC (Lossless or Hi-Res)
				split := strings.Split(variant.Audio, "-")
				if len(split) >= 3 {
					bitDepth := split[len(split)-1]
					sampleRate := split[len(split)-2]
					sampleRateInt, _ := strconv.Atoi(sampleRate)
					if sampleRateInt > 48000 { // Hi-Res
						hasHiRes = true
						hiResQuality = fmt.Sprintf("ALAC | 2 Channel | %s-bit/%d kHz", bitDepth, sampleRateInt/1000)
					} else { // Standard Lossless
						hasLossless = true
						losslessQuality = fmt.Sprintf("ALAC | 2 Channel | %s-bit/%d kHz", bitDepth, sampleRateInt/1000)
					}
				}
			} else if variant.Codecs == "ac-3" { // Dolby Audio
				hasDolbyAudio = true
				split := strings.Split(variant.Audio, "-")
				if len(split) > 0 {
					bitrate, _ := strconv.Atoi(split[len(split)-1])
					dolbyAudioQuality = fmt.Sprintf("AC-3 |  16 Channel | %d Kbps", bitrate)
				}
			}
		}

		fmt.Println("Available Audio Formats:")
		fmt.Println("------------------------")
		fmt.Printf("AAC             : %s\n", formatAvailability(hasAAC, aacQuality))
		fmt.Printf("Lossless        : %s\n", formatAvailability(hasLossless, losslessQuality))
		fmt.Printf("Hi-Res Lossless : %s\n", formatAvailability(hasHiRes, hiResQuality))
		fmt.Printf("Dolby Atmos     : %s\n", formatAvailability(hasAtmos, atmosQuality))
		fmt.Printf("Dolby Audio     : %s\n", formatAvailability(hasDolbyAudio, dolbyAudioQuality))
		fmt.Println("------------------------")

		return "", "", nil
	}
	var Quality string
	for _, variant := range master.Variants {
		if dl_atmos {
			if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") {
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found Dolby Atmos variant - %s (Bitrate: %d Kbps)\n",
						variant.Audio, variant.Bandwidth/1000)
				}
				split := strings.Split(variant.Audio, "-")
				length := len(split)
				length_int, err := strconv.Atoi(split[length-1])
				if err != nil {
					return "", "", err
				}
				if length_int <= Config.AtmosMax {
					if !debug_mode && !more_mode {
						fmt.Printf("%s\n", variant.Audio)
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						return "", "", err
					}
					streamUrl = streamUrlTemp
					Quality = fmt.Sprintf("%s Kbps", split[len(split)-1])
					break
				}
			} else if variant.Codecs == "ac-3" { // Add Dolby Audio support
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found Dolby Audio variant - %s (Bitrate: %d Kbps)\n",
						variant.Audio, variant.Bandwidth/1000)
				}
				streamUrlTemp, err := masterUrl.Parse(variant.URI)
				if err != nil {
					return "", "", err
				}
				streamUrl = streamUrlTemp
				split := strings.Split(variant.Audio, "-")
				Quality = fmt.Sprintf("%s Kbps", split[len(split)-1])
				break
			}
		} else if dl_aac {
			if variant.Codecs == "mp4a.40.2" {
				if debug_mode && !more_mode {
					fmt.Printf("Debug: Found AAC variant - %s (Bitrate: %d)\n", variant.Audio, variant.Bandwidth)
				}
				aacregex := regexp.MustCompile(`audio-stereo-\d+`)
				replaced := aacregex.ReplaceAllString(variant.Audio, "aac")
				if replaced == Config.AacType {
					if !debug_mode && !more_mode {
						fmt.Printf("%s\n", variant.Audio)
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						panic(err)
					}
					streamUrl = streamUrlTemp
					split := strings.Split(variant.Audio, "-")
					Quality = fmt.Sprintf("%s Kbps", split[2])
					break
				}
			}
		} else {
			if variant.Codecs == "alac" {
				split := strings.Split(variant.Audio, "-")
				length := len(split)
				length_int, err := strconv.Atoi(split[length-2])
				if err != nil {
					return "", "", err
				}
				if length_int <= Config.AlacMax {
					if !debug_mode && !more_mode {
						fmt.Printf("%s-bit / %s Hz\n", split[length-1], split[length-2])
					}
					streamUrlTemp, err := masterUrl.Parse(variant.URI)
					if err != nil {
						panic(err)
					}
					streamUrl = streamUrlTemp
					KHZ := float64(length_int) / 1000.0
					Quality = fmt.Sprintf("%sB-%.1fkHz", split[length-1], KHZ)
					break
				}
			}
		}
	}
	if streamUrl == nil {
		return "", "", errors.New("no codec found")
	}
	return streamUrl.String(), Quality, nil
}
func extractVideo(c string) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}

	resp, err := http.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	videoString := string(body)

	from, listType, err := m3u8.DecodeFrom(strings.NewReader(videoString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}

	video := from.(*m3u8.MasterPlaylist)

	re := regexp.MustCompile(`_(\d+)x(\d+)`)

	var streamUrl *url.URL
	sort.Slice(video.Variants, func(i, j int) bool {
		return video.Variants[i].AverageBandwidth > video.Variants[j].AverageBandwidth
	})

	maxHeight := Config.MVMax

	for _, variant := range video.Variants {
		matches := re.FindStringSubmatch(variant.URI)
		if len(matches) == 3 {
			height := matches[2]
			var h int
			_, err := fmt.Sscanf(height, "%d", &h)
			if err != nil {
				continue
			}
			if h <= maxHeight {
				streamUrl, err = MediaUrl.Parse(variant.URI)
				if err != nil {
					return "", err
				}
				fmt.Println("Video: " + variant.Resolution + "-" + variant.VideoRange)
				break
			}
		}
	}

	if streamUrl == nil {
		return "", errors.New("no suitable video stream found")
	}

	return streamUrl.String(), nil
}

func ripSong(songId string, token string, storefront string, mediaUserToken string) error {
	// Get song info to find album ID
	manifest, err := ampapi.GetSongResp(storefront, songId, Config.Language, token)
	if err != nil {
		fmt.Println("Failed to get song response.")
		return err
	}

	songData := manifest.Data[0]
	albumId := songData.Relationships.Albums.Data[0].ID

	// Use album approach but only download the specific song
	dl_song = true
	err = ripAlbum(albumId, token, storefront, mediaUserToken, songId)
	if err != nil {
		fmt.Println("Failed to rip song:", err)
		return err
	}

	return nil
}



const (
	defaultSearchLimit = 8
	defaultQueueSize  = 20
	pendingTTL         = 10 * time.Minute
	defaultTelegramFormat = "alac"
	defaultTelegramDownloadMaxGB = 3
)

const (
	telegramFormatAlac = "alac"
	telegramFormatFlac = "flac"
)

type TelegramBot struct {
	token        string
	apiBase      string
	appleToken   string
	client       *http.Client
	allowedChats map[int64]bool
	searchLimit  int
	maxFileBytes int64

	formatMu    sync.Mutex
	chatFormats map[int64]string

	pendingMu sync.Mutex
	pending   map[int64]*PendingSelection

	queueMu       sync.Mutex
	downloadQueue chan *downloadRequest
	inProgress    bool

	cacheMu   sync.Mutex
	cacheFile string
	cache     map[string]CachedAudio
}

type PendingSelection struct {
	Kind             string
	Query            string
	Title            string
	Offset           int
	HasNext          bool
	Items            []apputils.SearchResultItem
	CreatedAt        time.Time
	ReplyToMessageID int
	ResultsMessageID int
}

type downloadRequest struct {
	chatID    int64
	replyToID int
	single    bool
	format    string
	fn        func() error
}

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	InlineQuery   *InlineQuery   `json:"inline_query,omitempty"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from,omitempty"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type InlineQuery struct {
	ID    string `json:"id"`
	From  *User  `json:"from,omitempty"`
	Query string `json:"query"`
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text                        string `json:"text"`
	CallbackData                string `json:"callback_data,omitempty"`
	SwitchInlineQuery           *string `json:"switch_inline_query,omitempty"`
	SwitchInlineQueryCurrentChat *string `json:"switch_inline_query_current_chat,omitempty"`
	Url                         string `json:"url,omitempty"`
}

type ReplyKeyboardMarkup struct {
	Keyboard        [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool               `json:"resize_keyboard,omitempty"`
	OneTimeKeyboard bool               `json:"one_time_keyboard,omitempty"`
}

type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

type KeyboardButton struct {
	Text string `json:"text"`
}

type getUpdatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []Update `json:"result"`
	Description string   `json:"description,omitempty"`
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
}

type sendMessageResponse struct {
	OK          bool    `json:"ok"`
	Result      Message `json:"result"`
	Description string  `json:"description,omitempty"`
}

type sendAudioResponse struct {
	OK          bool   `json:"ok"`
	Result      AudioMessage `json:"result"`
	Description string `json:"description,omitempty"`
}

type AudioMessage struct {
	MessageID int   `json:"message_id"`
	Audio     Audio `json:"audio"`
}

type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type InlineQueryResultCachedAudio struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	AudioFileID string `json:"audio_file_id"`
	Caption     string `json:"caption,omitempty"`
}

type InlineQueryResultArticle struct {
	Type                string              `json:"type"`
	ID                  string              `json:"id"`
	Title               string              `json:"title"`
	Description         string              `json:"description,omitempty"`
	InputMessageContent InputMessageContent `json:"input_message_content"`
}

type InputMessageContent struct {
	MessageText string `json:"message_text"`
}

func runTelegramBot(appleToken string) {
	botToken := strings.TrimSpace(Config.TelegramBotToken)
	if botToken == "" {
		botToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	}
	if botToken == "" {
		fmt.Println("telegram-bot-token is not set. Add it to config.yaml or TELEGRAM_BOT_TOKEN.")
		return
	}
	if Config.TelegramDownloadFolder != "" {
		Config.AlacSaveFolder = Config.TelegramDownloadFolder
	}

	bot := newTelegramBot(botToken, appleToken)
	fmt.Println("Telegram bot started. Waiting for updates...")
	bot.loop()
}

func normalizeTelegramAPIBase(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "https://api.telegram.org"
	}
	return strings.TrimRight(base, "/")
}

func telegramDownloadMaxBytes() int64 {
	gb := Config.TelegramDownloadMaxGB
	if gb <= 0 {
		gb = defaultTelegramDownloadMaxGB
	}
	return int64(gb) * 1024 * 1024 * 1024
}

func newTelegramBot(token, appleToken string) *TelegramBot {
	allowed := make(map[int64]bool)
	for _, id := range Config.TelegramAllowedChatIDs {
		allowed[id] = true
	}
	searchLimit := Config.TelegramSearchLimit
	if searchLimit <= 0 {
		searchLimit = defaultSearchLimit
	}
	maxFileBytes := int64(Config.TelegramMaxFileMB) * 1024 * 1024
	if maxFileBytes <= 0 {
		maxFileBytes = 50 * 1024 * 1024
	}
	cacheFile := strings.TrimSpace(Config.TelegramCacheFile)
	if cacheFile == "" {
		cacheFile = "telegram-cache.json"
	}
	queueSize := defaultQueueSize
	if queueSize <= 0 {
		queueSize = 1
	}
	apiBase := normalizeTelegramAPIBase(Config.TelegramAPIURL)
	bot := &TelegramBot{
		token:         token,
		apiBase:       apiBase,
		appleToken:    appleToken,
		client:        &http.Client{Timeout: 60 * time.Second},
		allowedChats:  allowed,
		searchLimit:   searchLimit,
		maxFileBytes:  maxFileBytes,
		chatFormats:   make(map[int64]string),
		pending:       make(map[int64]*PendingSelection),
		downloadQueue: make(chan *downloadRequest, queueSize),
		cacheFile:     cacheFile,
		cache:         make(map[string]CachedAudio),
	}
	bot.loadCache()
	bot.startDownloadWorker()
	return bot
}

func (b *TelegramBot) loop() {
	offset := 0
	for {
		updates, err := b.getUpdates(offset)
		if err != nil {
			fmt.Println("getUpdates error:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}
			if upd.Message != nil {
				b.handleMessage(upd.Message)
			} else if upd.CallbackQuery != nil {
				b.handleCallback(upd.CallbackQuery)
			} else if upd.InlineQuery != nil {
				b.handleInlineQuery(upd.InlineQuery)
			}
		}
	}
}

func (b *TelegramBot) startDownloadWorker() {
	go func() {
		for req := range b.downloadQueue {
			b.queueMu.Lock()
			b.inProgress = true
			b.queueMu.Unlock()

			b.runDownload(req.chatID, req.fn, req.single, req.replyToID, req.format)

			b.queueMu.Lock()
			b.inProgress = false
			b.queueMu.Unlock()
		}
	}()
}

func normalizeTelegramFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case telegramFormatAlac:
		return telegramFormatAlac
	case telegramFormatFlac:
		return telegramFormatFlac
	default:
		return ""
	}
}

func (b *TelegramBot) getChatFormat(chatID int64) string {
	b.formatMu.Lock()
	defer b.formatMu.Unlock()
	if b.chatFormats == nil {
		b.chatFormats = make(map[int64]string)
	}
	if format, ok := b.chatFormats[chatID]; ok {
		if normalized := normalizeTelegramFormat(format); normalized != "" {
			return normalized
		}
	}
	return defaultTelegramFormat
}

func (b *TelegramBot) setChatFormat(chatID int64, format string) string {
	normalized := normalizeTelegramFormat(format)
	if normalized == "" {
		return ""
	}
	b.formatMu.Lock()
	defer b.formatMu.Unlock()
	if b.chatFormats == nil {
		b.chatFormats = make(map[int64]string)
	}
	b.chatFormats[chatID] = normalized
	return normalized
}

func (b *TelegramBot) cacheKey(trackID, format string, compressed bool) string {
	normalized := normalizeTelegramFormat(format)
	if normalized == "" {
		normalized = telegramFormatFlac
	}
	return fmt.Sprintf("%s|%s|%t", trackID, normalized, compressed)
}

func (b *TelegramBot) loadCache() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cache = make(map[string]CachedAudio)
	if b.cacheFile == "" {
		return
	}
	data, err := os.ReadFile(b.cacheFile)
	if err != nil {
		return
	}
	var payload telegramCacheFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	if payload.Items == nil {
		return
	}
	if payload.Version < 2 {
		migrated := make(map[string]CachedAudio)
		for key, entry := range payload.Items {
			parts := strings.Split(key, "|")
			if len(parts) == 2 {
				trackID := parts[0]
				compressed, err := strconv.ParseBool(parts[1])
				if err != nil {
					continue
				}
				entry.Compressed = compressed
				if entry.Format == "" {
					entry.Format = telegramFormatFlac
				}
				migrated[b.cacheKey(trackID, entry.Format, entry.Compressed)] = entry
				continue
			}
			if len(parts) >= 3 {
				trackID := parts[0]
				format := normalizeTelegramFormat(parts[1])
				compressed, err := strconv.ParseBool(parts[2])
				if err != nil {
					continue
				}
				if format == "" {
					format = telegramFormatFlac
				}
				entry.Compressed = compressed
				if entry.Format == "" {
					entry.Format = format
				}
				migrated[b.cacheKey(trackID, format, entry.Compressed)] = entry
			}
		}
		b.cache = migrated
		b.saveCacheLocked()
		return
	}
	b.cache = payload.Items
	for key, entry := range b.cache {
		if entry.Format == "" {
			parts := strings.Split(key, "|")
			if len(parts) >= 2 {
				entry.Format = normalizeTelegramFormat(parts[1])
			}
			if entry.Format == "" {
				entry.Format = telegramFormatFlac
			}
			b.cache[key] = entry
		}
	}
}

func (b *TelegramBot) saveCacheLocked() {
	if b.cacheFile == "" {
		return
	}
	dir := filepath.Dir(b.cacheFile)
	if dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	payload := telegramCacheFile{
		Version: 2,
		Items:   b.cache,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	tmp := b.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, b.cacheFile)
}

func (b *TelegramBot) fetchTrackMeta(trackID string) (AudioMeta, error) {
	if trackID == "" {
		return AudioMeta{}, fmt.Errorf("empty track id")
	}
	resp, err := ampapi.GetSongResp(Config.Storefront, trackID, b.searchLanguage(), b.appleToken)
	if err != nil || resp == nil || len(resp.Data) == 0 {
		if err != nil {
			return AudioMeta{}, err
		}
		return AudioMeta{}, fmt.Errorf("empty song response")
	}
	data := resp.Data[0]
	return AudioMeta{
		TrackID:        trackID,
		Title:          strings.TrimSpace(data.Attributes.Name),
		Performer:      strings.TrimSpace(data.Attributes.ArtistName),
		DurationMillis: int64(data.Attributes.DurationInMillis),
	}, nil
}

func (b *TelegramBot) enrichCachedAudio(trackID string, entry CachedAudio) CachedAudio {
	updated := false
	sizeBytes := entry.SizeBytes
	if sizeBytes <= 0 {
		sizeBytes = entry.FileSize
		if sizeBytes > 0 {
			entry.SizeBytes = sizeBytes
			updated = true
		}
	}
	if trackID != "" && (entry.DurationMillis <= 0 || entry.Title == "" || entry.Performer == "") {
		if meta, err := b.fetchTrackMeta(trackID); err == nil {
			if entry.DurationMillis <= 0 && meta.DurationMillis > 0 {
				entry.DurationMillis = meta.DurationMillis
				updated = true
			}
			if entry.Title == "" && meta.Title != "" {
				entry.Title = meta.Title
				updated = true
			}
			if entry.Performer == "" && meta.Performer != "" {
				entry.Performer = meta.Performer
				updated = true
			}
		}
	}
	if entry.BitrateKbps <= 0 && sizeBytes > 0 && entry.DurationMillis > 0 {
		entry.BitrateKbps = calcBitrateKbps(sizeBytes, entry.DurationMillis)
		updated = true
	}
	if updated && trackID != "" {
		b.storeCachedAudio(trackID, entry)
	}
	return entry
}

func (b *TelegramBot) storeCachedAudio(trackID string, entry CachedAudio) {
	if trackID == "" || entry.FileID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cache == nil {
		b.cache = make(map[string]CachedAudio)
	}
	entry.Format = normalizeTelegramFormat(entry.Format)
	if entry.Format == "" {
		entry.Format = telegramFormatFlac
	}
	entry.UpdatedAt = time.Now()
	b.cache[b.cacheKey(trackID, entry.Format, entry.Compressed)] = entry
	b.saveCacheLocked()
}

func (b *TelegramBot) deleteCachedAudio(trackID, format string, compressed bool) {
	if trackID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cache == nil {
		return
	}
	delete(b.cache, b.cacheKey(trackID, format, compressed))
	b.saveCacheLocked()
}

func (b *TelegramBot) getCachedAudio(trackID string, maxBytes int64, format string) (CachedAudio, bool) {
	if trackID == "" {
		return CachedAudio{}, false
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cache == nil {
		return CachedAudio{}, false
	}
	var candidates []CachedAudio
	normalized := normalizeTelegramFormat(format)
	if normalized != "" {
		if entry, ok := b.cache[b.cacheKey(trackID, normalized, false)]; ok {
			if entry.Format == "" {
				entry.Format = normalized
			}
			candidates = append(candidates, entry)
		}
		if entry, ok := b.cache[b.cacheKey(trackID, normalized, true)]; ok {
			if entry.Format == "" {
				entry.Format = normalized
			}
			candidates = append(candidates, entry)
		}
	} else {
		prefix := trackID + "|"
		for key, entry := range b.cache {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if entry.Format == "" {
				parts := strings.Split(key, "|")
				if len(parts) >= 3 {
					entry.Format = normalizeTelegramFormat(parts[1])
				}
				if entry.Format == "" {
					entry.Format = telegramFormatFlac
				}
			}
			candidates = append(candidates, entry)
		}
	}
	var best *CachedAudio
	for _, entry := range candidates {
		entrySize := entry.SizeBytes
		if entrySize <= 0 {
			entrySize = entry.FileSize
		}
		if maxBytes > 0 && entrySize > 0 && entrySize > maxBytes {
			continue
		}
		if best == nil {
			copyEntry := entry
			best = &copyEntry
			continue
		}
		if best.Compressed && !entry.Compressed {
			copyEntry := entry
			best = &copyEntry
			continue
		}
		bestSize := best.SizeBytes
		if bestSize <= 0 {
			bestSize = best.FileSize
		}
		if best.Compressed == entry.Compressed && entrySize > bestSize {
			copyEntry := entry
			best = &copyEntry
		}
	}
	if best == nil {
		return CachedAudio{}, false
	}
	return *best, true
}

func (b *TelegramBot) handleMessage(msg *Message) {
	if msg.Text == "" {
		return
	}
	if !b.isAllowedChat(msg.Chat.ID) {
		_ = b.sendMessage(msg.Chat.ID, "Not authorized for this bot.", nil)
		return
	}
	text := strings.TrimSpace(msg.Text)
	if cmd, args, ok := parseCommand(text); ok {
		b.handleCommand(msg.Chat.ID, cmd, args, msg.MessageID)
		return
	}
}

func (b *TelegramBot) handleCallback(cb *CallbackQuery) {
	if cb == nil || cb.Message == nil {
		return
	}
	if !b.isAllowedChat(cb.Message.Chat.ID) {
		return
	}
	data := strings.TrimSpace(cb.Data)
	if strings.HasPrefix(data, "sel:") {
		numStr := strings.TrimPrefix(data, "sel:")
		if n, err := strconv.Atoi(numStr); err == nil {
			b.handleSelection(cb.Message.Chat.ID, cb.Message.MessageID, n)
		}
	} else if strings.HasPrefix(data, "setting:") {
		format := strings.TrimPrefix(data, "setting:")
		if normalized := b.setChatFormat(cb.Message.Chat.ID, format); normalized != "" {
			text := fmt.Sprintf("Download format set to %s.", strings.ToUpper(normalized))
			_ = b.editMessageText(cb.Message.Chat.ID, cb.Message.MessageID, text, buildSettingsKeyboard(normalized))
		}
	} else if strings.HasPrefix(data, "page:") {
		deltaStr := strings.TrimPrefix(data, "page:")
		if delta, err := strconv.Atoi(deltaStr); err == nil {
			b.handlePage(cb.Message.Chat.ID, cb.Message.MessageID, delta)
		}
	}
	_ = b.answerCallbackQuery(cb.ID)
}

func (b *TelegramBot) handleInlineQuery(q *InlineQuery) {
	if q == nil || q.ID == "" {
		return
	}
	query := strings.TrimSpace(q.Query)
	if query == "" {
		_ = b.answerInlineQuery(q.ID, []any{}, true)
		return
	}
	trackID := extractInlineTrackID(query)
	if trackID == "" {
		_ = b.answerInlineQuery(q.ID, []any{}, true)
		return
	}
	entry, ok := b.getCachedAudio(trackID, b.maxFileBytes, "")
	results := []any{}
	if ok {
		entry = b.enrichCachedAudio(trackID, entry)
		format := normalizeTelegramFormat(entry.Format)
		if format == "" {
			format = telegramFormatFlac
		}
		results = append(results, InlineQueryResultCachedAudio{
			Type:        "audio",
			ID:          fmt.Sprintf("song_%s", trackID),
			AudioFileID: entry.FileID,
			Caption:     formatTelegramCaption(entry.SizeBytes, entry.BitrateKbps, format),
		})
	} else {
		meta, err := b.fetchTrackMeta(trackID)
		title := "Send /songid " + trackID
		description := ""
		if err == nil {
			if meta.Title != "" && meta.Performer != "" {
				title = meta.Performer + " - " + meta.Title
				description = "Send /songid " + trackID
			} else if meta.Title != "" {
				title = meta.Title
				description = "Send /songid " + trackID
			}
		}
		results = append(results, InlineQueryResultArticle{
			Type:        "article",
			ID:          fmt.Sprintf("songcmd_%s", trackID),
			Title:       title,
			Description: description,
			InputMessageContent: InputMessageContent{
				MessageText: "/songid " + trackID,
			},
		})
	}
	_ = b.answerInlineQuery(q.ID, results, true)
}

func (b *TelegramBot) handleCommand(chatID int64, cmd string, args []string, replyToID int) {
	switch cmd {
	case "start", "help":
		_ = b.sendMessage(chatID, botHelpText(), nil)
	case "search_song":
		b.handleSearch(chatID, "song", strings.Join(args, " "), replyToID)
	case "search_album":
		b.handleSearch(chatID, "album", strings.Join(args, " "), replyToID)
	case "search_artist":
		b.handleSearch(chatID, "artist", strings.Join(args, " "), replyToID)
	case "search":
		if len(args) < 2 {
			_ = b.sendMessageWithReply(chatID, "Usage: /search <song|album|artist> <keywords>", nil, replyToID)
			return
		}
		kind := strings.ToLower(args[0])
		b.handleSearch(chatID, kind, strings.Join(args[1:], " "), replyToID)
	case "id":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /id <song|album> <id>", nil)
			return
		}
		if len(args) == 1 {
			b.queueDownloadSong(chatID, args[0])
			return
		}
		switch strings.ToLower(args[0]) {
		case "song":
			b.queueDownloadSong(chatID, args[1])
		case "album":
			b.queueDownloadAlbum(chatID, args[1])
		default:
			_ = b.sendMessage(chatID, "Usage: /id <song|album> <id>", nil)
		}
	case "songid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /songid <id>", nil)
			return
		}
		b.queueDownloadSong(chatID, args[0])
	case "albumid":
		if len(args) == 0 {
			_ = b.sendMessage(chatID, "Usage: /albumid <id>", nil)
			return
		}
		b.queueDownloadAlbum(chatID, args[0])
	case "settings":
		if len(args) > 0 {
			normalized := normalizeTelegramFormat(args[0])
			if normalized == "" {
				_ = b.sendMessageWithReply(chatID, "Usage: /settings <alac|flac>", nil, replyToID)
				return
			}
			b.setChatFormat(chatID, normalized)
			_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Download format set to %s.", strings.ToUpper(normalized)), buildSettingsKeyboard(normalized), replyToID)
			return
		}
		current := b.getChatFormat(chatID)
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Download format: %s", strings.ToUpper(current)), buildSettingsKeyboard(current), replyToID)
	default:
		_ = b.sendMessage(chatID, "Unknown command. Send /help for usage.", nil)
	}
}

func (b *TelegramBot) handleSearch(chatID int64, kind string, query string, replyToID int) {
	query = strings.TrimSpace(query)
	if query == "" {
		_ = b.sendMessageWithReply(chatID, "Please provide a search query.", nil, replyToID)
		return
	}
	kind = strings.ToLower(kind)
	if kind != "song" && kind != "album" && kind != "artist" {
		_ = b.sendMessageWithReply(chatID, "Search type must be song, album, or artist.", nil, replyToID)
		return
	}
	offset := 0
	items, hasNext, err := b.fetchSearchPage(kind, query, offset)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Search failed: %v", err), nil, replyToID)
		return
	}
	if len(items) == 0 {
		_ = b.sendMessageWithReply(chatID, "No results found.", nil, replyToID)
		return
	}
	message := apputils.FormatSearchResults(kind, query, items)
	messageID, err := b.sendMessageWithReplyReturn(chatID, message, buildInlineKeyboard(len(items), offset > 0, hasNext), replyToID)
	if err != nil {
		return
	}
	b.setPending(chatID, kind, query, offset, items, hasNext, replyToID, messageID, "")
}

func (b *TelegramBot) searchLanguage() string {
	lang := strings.TrimSpace(Config.TelegramSearchLanguage)
	if lang == "" {
		lang = strings.TrimSpace(Config.Language)
	}
	return lang
}

func (b *TelegramBot) fetchSearchPage(kind string, query string, offset int) ([]apputils.SearchResultItem, bool, error) {
	apiType := kind + "s"
	resp, err := ampapi.Search(Config.Storefront, query, apiType, b.searchLanguage(), b.appleToken, b.searchLimit, offset)
	if err != nil {
		return nil, false, err
	}
	items, hasNext := apputils.BuildSearchItems(kind, resp)
	return items, hasNext, nil
}

func (b *TelegramBot) handleSelection(chatID int64, messageID int, choice int) {
	pending, ok := b.getPending(chatID)
	if !ok {
		_ = b.sendMessage(chatID, "No active selection. Start with /search_song or /search_album.", nil)
		return
	}
	if pending.ResultsMessageID != 0 && messageID != pending.ResultsMessageID {
		return
	}
	replyToID := pending.ReplyToMessageID
	if time.Since(pending.CreatedAt) > pendingTTL {
		b.clearPending(chatID)
		_ = b.sendMessageWithReply(chatID, "Selection expired. Please search again.", nil, replyToID)
		return
	}
	if choice < 1 || choice > len(pending.Items) {
		_ = b.sendMessageWithReply(chatID, "Selection out of range.", nil, replyToID)
		return
	}
	selected := pending.Items[choice-1]
	switch pending.Kind {
	case "song":
		setSearchMeta(selected.ID, selected.Name, selected.Artist)
		b.queueDownloadSongWithReply(chatID, selected.ID, replyToID)
	case "album", "artist_album":
		b.queueDownloadAlbumWithReply(chatID, selected.ID, replyToID)
	case "artist":
		albums, hasNext, err := apputils.FetchArtistAlbums(Config.Storefront, selected.ID, b.appleToken, b.searchLimit, 0, b.searchLanguage())
		if err != nil {
			_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to load artist albums: %v", err), nil, replyToID)
			return
		}
		if len(albums) == 0 {
			_ = b.sendMessageWithReply(chatID, "No albums found for this artist.", nil, replyToID)
			return
		}
		message := apputils.FormatArtistAlbums(selected.Name, albums)
		messageID, err := b.sendMessageWithReplyReturn(chatID, message, buildInlineKeyboard(len(albums), false, hasNext), replyToID)
		if err != nil {
			return
		}
		b.setPending(chatID, "artist_album", selected.ID, 0, albums, hasNext, replyToID, messageID, selected.Name)
	default:
		b.clearPending(chatID)
	}
}

func (b *TelegramBot) handlePage(chatID int64, messageID int, delta int) {
	pending, ok := b.getPending(chatID)
	if !ok {
		return
	}
	if pending.ResultsMessageID != messageID {
		return
	}
	if pending.Query == "" {
		return
	}
	newOffset := pending.Offset + delta*b.searchLimit
	if newOffset < 0 {
		return
	}
	var (
		items   []apputils.SearchResultItem
		hasNext bool
		err     error
		message string
	)
	switch pending.Kind {
	case "song", "album", "artist":
		items, hasNext, err = b.fetchSearchPage(pending.Kind, pending.Query, newOffset)
		if err != nil {
			_ = b.editMessageText(chatID, messageID, fmt.Sprintf("Search failed: %v", err), nil)
			return
		}
		if len(items) == 0 {
			return
		}
		message = apputils.FormatSearchResults(pending.Kind, pending.Query, items)
	case "artist_album":
		items, hasNext, err = apputils.FetchArtistAlbums(Config.Storefront, pending.Query, b.appleToken, b.searchLimit, newOffset, b.searchLanguage())
		if err != nil {
			_ = b.editMessageText(chatID, messageID, fmt.Sprintf("Failed to load artist albums: %v", err), nil)
			return
		}
		if len(items) == 0 {
			return
		}
		message = apputils.FormatArtistAlbums(pending.Title, items)
	default:
		return
	}
	_ = b.editMessageText(chatID, messageID, message, buildInlineKeyboard(len(items), newOffset > 0, hasNext))
	b.setPending(chatID, pending.Kind, pending.Query, newOffset, items, hasNext, pending.ReplyToMessageID, messageID, pending.Title)
}

func (b *TelegramBot) queueDownloadSong(chatID int64, songID string) {
	b.queueDownloadSongWithReply(chatID, songID, 0)
}

func (b *TelegramBot) queueDownloadSongWithReply(chatID int64, songID string, replyToID int) {
	if songID == "" {
		_ = b.sendMessage(chatID, "Song ID is empty.", nil)
		return
	}
	format := b.getChatFormat(chatID)
	if b.trySendCachedTrack(chatID, replyToID, songID, format) {
		return
	}
	b.enqueueDownload(chatID, replyToID, true, format, func() error {
		return ripSong(songID, b.appleToken, Config.Storefront, Config.MediaUserToken)
	})
}

func (b *TelegramBot) queueDownloadAlbum(chatID int64, albumID string) {
	b.queueDownloadAlbumWithReply(chatID, albumID, 0)
}

func (b *TelegramBot) queueDownloadAlbumWithReply(chatID int64, albumID string, replyToID int) {
	if albumID == "" {
		_ = b.sendMessage(chatID, "Album ID is empty.", nil)
		return
	}
	format := b.getChatFormat(chatID)
	b.enqueueDownload(chatID, replyToID, false, format, func() error {
		return ripAlbum(albumID, b.appleToken, Config.Storefront, Config.MediaUserToken, "")
	})
}

func (b *TelegramBot) enqueueDownload(chatID int64, replyToID int, single bool, format string, fn func() error) {
	req := &downloadRequest{
		chatID:    chatID,
		replyToID: replyToID,
		single:    single,
		format:    format,
		fn:        fn,
	}
	b.queueMu.Lock()
	inProgress := b.inProgress
	queueLen := len(b.downloadQueue)
	queueCap := cap(b.downloadQueue)
	position := queueLen + 1
	if inProgress {
		position++
	}
	queueFull := queueLen >= queueCap
	b.queueMu.Unlock()

	if queueFull {
		_ = b.sendMessageWithReply(chatID, "Download queue is full. Please try again later.", nil, replyToID)
		return
	}
	select {
	case b.downloadQueue <- req:
	default:
		_ = b.sendMessageWithReply(chatID, "Download queue is full. Please try again later.", nil, replyToID)
		return
	}
	if inProgress || queueLen > 0 {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Queued. Position: %d", position), nil, replyToID)
	}
}

func (b *TelegramBot) trySendCachedTrack(chatID int64, replyToID int, trackID string, format string) bool {
	entry, ok := b.getCachedAudio(trackID, b.maxFileBytes, format)
	if !ok {
		return false
	}
	if err := b.sendAudioByFileID(chatID, entry, replyToID, trackID); err != nil {
		b.deleteCachedAudio(trackID, entry.Format, entry.Compressed)
		return false
	}
	return true
}

func (b *TelegramBot) runDownload(chatID int64, fn func() error, single bool, replyToID int, format string) {

	lastDownloadedPaths = nil
	downloadedMetaMu.Lock()
	downloadedMeta = make(map[string]AudioMeta)
	downloadedMetaMu.Unlock()
	counter = structs.Counter{}
	okDict = make(map[string][]int)

	dl_atmos = false
	dl_aac = false
	dl_select = false
	if single {
		dl_song = true
	} else {
		dl_song = false
	}

	format = normalizeTelegramFormat(format)
	if format == "" {
		format = defaultTelegramFormat
	}
	defer b.cleanupDownloadsIfNeeded()
	Config.ConvertAfterDownload = format == telegramFormatFlac
	if format == telegramFormatFlac {
		Config.ConvertFormat = telegramFormatFlac
		Config.ConvertKeepOriginal = false
		Config.ConvertSkipLossyToLossless = false
		if _, err := exec.LookPath(Config.FFmpegPath); err != nil {
			_ = b.sendMessageWithReply(chatID, fmt.Sprintf("ffmpeg not found at '%s'.", Config.FFmpegPath), nil, replyToID)
			dl_song = false
			return
		}
	} else {
		Config.ConvertFormat = ""
	}

	status, err := newDownloadStatus(b, chatID, replyToID)
	if err != nil {
		_ = b.sendMessageWithReply(chatID, fmt.Sprintf("Failed to create status message: %v", err), nil, replyToID)
		dl_song = false
		return
	}
	defer status.Stop()

	progress := func(phase string, done, total int64) {
		status.Update(phase, done, total)
	}
	activeProgress = progress
	defer func() { activeProgress = nil }()

	status.Update("Downloading", 0, 0)
	err = fn()
	if err != nil {
		status.UpdateSync(fmt.Sprintf("Failed: %v", err), 0, 0)
		dl_song = false
		return
	}
	dl_song = false

	activeProgress = nil

	paths := append([]string{}, lastDownloadedPaths...)
	if len(paths) == 0 {
		status.UpdateSync("No files were downloaded.", 0, 0)
		return
	}
	sentAny := false
	for _, path := range paths {
		if err := b.sendAudioFile(chatID, path, replyToID, status, format); err != nil {
			status.Update(fmt.Sprintf("Failed to send audio: %v", err), 0, 0)
			continue
		}
		sentAny = true
	}
	if sentAny {
		status.Stop()
		_ = b.deleteMessage(chatID, status.messageID)
	}
}

type downloadFileEntry struct {
	path    string
	size    int64
	modTime time.Time
}

func (b *TelegramBot) cleanupDownloadsIfNeeded() {
	root := strings.TrimSpace(Config.AlacSaveFolder)
	if root == "" {
		return
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == string(filepath.Separator) {
		fmt.Printf("Skip cleanup for unsafe download folder: %s\n", root)
		return
	}
	info, err := os.Stat(cleanRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		fmt.Printf("Download folder check failed: %v\n", err)
		return
	}
	if !info.IsDir() {
		return
	}
	totalSize, files, err := scanDownloadFolder(cleanRoot, Config.TelegramCacheFile)
	if err != nil {
		fmt.Printf("Download folder scan failed: %v\n", err)
		return
	}
	maxBytes := telegramDownloadMaxBytes()
	if totalSize <= maxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, entry := range files {
		if totalSize <= maxBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil {
			continue
		}
		totalSize -= entry.size
	}
}

func scanDownloadFolder(root string, cacheFile string) (int64, []downloadFileEntry, error) {
	var totalSize int64
	entries := []downloadFileEntry{}
	cachePath := ""
	if cacheFile != "" {
		cachePath = filepath.Clean(cacheFile)
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if cachePath != "" && filepath.Clean(path) == cachePath {
			return nil
		}
		size := info.Size()
		totalSize += size
		entries = append(entries, downloadFileEntry{
			path:    path,
			size:    size,
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return totalSize, entries, err
	}
	return totalSize, entries, nil
}

func (b *TelegramBot) sendAudioFile(chatID int64, filePath string, replyToID int, status *DownloadStatus, format string) error {
	format = normalizeTelegramFormat(format)
	if format == "" {
		format = defaultTelegramFormat
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	switch format {
	case telegramFormatFlac:
		if ext != ".flac" {
			return fmt.Errorf("output is not FLAC: %s", filepath.Base(filePath))
		}
	case telegramFormatAlac:
		if ext != ".m4a" && ext != ".mp4" {
			return fmt.Errorf("output is not ALAC: %s", filepath.Base(filePath))
		}
	}
	sendPath := filePath
	displayName := filepath.Base(filePath)
	thumbPath := ""
	compressed := false
	meta, hasMeta := getDownloadedMeta(filePath)
	cleanup := func() {
		if thumbPath != "" {
			_ = os.Remove(thumbPath)
		}
	}
	defer cleanup()

	info, err := os.Stat(sendPath)
	if err != nil {
		return err
	}
	if info.Size() > b.maxFileBytes {
		if format != telegramFormatFlac {
			return fmt.Errorf("ALAC file exceeds Telegram limit (%dMB). Use /settings flac or raise telegram-max-file-mb.", b.maxFileBytes/1024/1024)
		}
		if status != nil {
			status.Update("Compressing", 0, 0)
		}
		compressedPath, err := b.compressFlacToSize(sendPath, b.maxFileBytes)
		if err != nil {
			return err
		}
		sendPath = compressedPath
		compressed = true
		cleanup = func() {
			_ = os.Remove(compressedPath)
		}
		info, err = os.Stat(sendPath)
		if err != nil {
			return err
		}
		if info.Size() > b.maxFileBytes {
			return fmt.Errorf("compressed file still too large: %s", filepath.Base(sendPath))
		}
	}
	file, err := os.Open(sendPath)
	if err != nil {
		return err
	}
	defer file.Close()

	sizeBytes := info.Size()
	durationMillis := int64(0)
	if hasMeta {
		durationMillis = meta.DurationMillis
	}
	bitrateKbps := calcBitrateKbps(sizeBytes, durationMillis)
	if bitrateKbps <= 0 {
		if seconds, err := getAudioDurationSeconds(sendPath); err == nil && seconds > 0 {
			durationMillis = int64(seconds * 1000.0)
			bitrateKbps = calcBitrateKbps(sizeBytes, durationMillis)
		}
	}
	caption := formatTelegramCaption(sizeBytes, bitrateKbps, format)
	if status != nil {
		status.Update("Uploading", 0, 0)
	}
	coverPath := findCoverFile(filepath.Dir(filePath))
	if coverPath != "" {
		if path, err := makeTelegramThumb(coverPath); err == nil {
			thumbPath = path
		}
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if replyToID > 0 {
		if err := writer.WriteField("reply_to_message_id", strconv.Itoa(replyToID)); err != nil {
			return err
		}
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return err
		}
	}
	if hasMeta {
		if meta.Title != "" {
			if err := writer.WriteField("title", meta.Title); err != nil {
				return err
			}
		}
		if meta.Performer != "" {
			if err := writer.WriteField("performer", meta.Performer); err != nil {
				return err
			}
		}
	}
	part, err := writer.CreateFormFile("audio", displayName)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, file); err != nil {
		return err
	}
	if thumbPath != "" {
		thumbFile, err := os.Open(thumbPath)
		if err == nil {
			defer thumbFile.Close()
			thumbPart, err := writer.CreateFormFile("thumbnail", filepath.Base(thumbPath))
			if err == nil {
				if _, err := io.Copy(thumbPart, thumbFile); err != nil {
					return err
				}
			}
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", b.apiURL("sendAudio"), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram sendAudio failed: %s", resp.Status)
	}
	apiResp := sendAudioResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram sendAudio error: %s", apiResp.Description)
	}
	if hasMeta && meta.TrackID != "" && apiResp.Result.Audio.FileID != "" {
		b.storeCachedAudio(meta.TrackID, CachedAudio{
			FileID:     apiResp.Result.Audio.FileID,
			FileSize:   apiResp.Result.Audio.FileSize,
			Compressed: compressed,
			Format:     format,
			SizeBytes:  sizeBytes,
			BitrateKbps: bitrateKbps,
			DurationMillis: durationMillis,
			Title:      meta.Title,
			Performer:  meta.Performer,
		})
	}
	return nil
}

type DownloadStatus struct {
	bot         *TelegramBot
	chatID      int64
	messageID   int
	lastPhase   string
	lastPercent int
	lastText    string
	lastUpdate  time.Time
	mu          sync.Mutex
	latestPhase string
	latestDone  int64
	latestTotal int64
	dirty       bool
	updateCh    chan struct{}
	stopCh      chan struct{}
	stopOnce    sync.Once
}

func newDownloadStatus(bot *TelegramBot, chatID int64, replyToID int) (*DownloadStatus, error) {
	messageID, err := bot.sendMessageWithReplyReturn(chatID, "Starting download...", nil, replyToID)
	if err != nil {
		return nil, err
	}
	status := &DownloadStatus{
		bot:       bot,
		chatID:    chatID,
		messageID: messageID,
		updateCh:  make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}
	go status.loop()
	return status, nil
}

func (s *DownloadStatus) Stop() {
	if s == nil || s.bot == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *DownloadStatus) Update(phase string, done, total int64) {
	if s == nil || s.bot == nil {
		return
	}
	s.mu.Lock()
	s.setLatestLocked(phase, done, total)
	s.mu.Unlock()
	select {
	case s.updateCh <- struct{}{}:
	default:
	}
}

func (s *DownloadStatus) UpdateSync(phase string, done, total int64) {
	if s == nil || s.bot == nil {
		return
	}
	s.mu.Lock()
	s.setLatestLocked(phase, done, total)
	s.mu.Unlock()
	s.flush(true)
}

func (s *DownloadStatus) setLatestLocked(phase string, done, total int64) {
	normalizedPhase := strings.TrimSpace(phase)
	if normalizedPhase == "" {
		normalizedPhase = "Working"
	}
	s.latestPhase = normalizedPhase
	s.latestDone = done
	s.latestTotal = total
	s.dirty = true
}

func (s *DownloadStatus) loop() {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.updateCh:
			s.flush(false)
		case <-ticker.C:
			s.flush(false)
		case <-s.stopCh:
			return
		}
	}
}

func (s *DownloadStatus) flush(force bool) {
	if s == nil || s.bot == nil {
		return
	}
	s.mu.Lock()
	if !s.dirty && !force {
		s.mu.Unlock()
		return
	}
	phase := s.latestPhase
	done := s.latestDone
	total := s.latestTotal
	s.dirty = false
	lastPhase := s.lastPhase
	lastPercent := s.lastPercent
	lastText := s.lastText
	lastUpdate := s.lastUpdate
	s.mu.Unlock()

	percent := -1
	if total > 0 {
		percent = int(float64(done) / float64(total) * 100)
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
	}

	text := formatProgressText(phase, done, total, percent)
	now := time.Now()
	phaseChanged := phase != lastPhase
	percentChanged := percent != lastPercent && percent >= 0
	if !force {
		if text == lastText {
			return
		}
		if !phaseChanged && !percentChanged && now.Sub(lastUpdate) < 2*time.Second {
			return
		}
	}

	if err := s.bot.editMessageText(s.chatID, s.messageID, text, nil); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.lastPhase = phase
	s.lastPercent = percent
	s.lastText = text
	s.lastUpdate = now
	s.mu.Unlock()
}

func formatProgressText(phase string, done, total int64, percent int) string {
	if total > 0 {
		if percent < 0 {
			percent = 0
		}
		return fmt.Sprintf("%s: %s / %s (%d%%)", phase, formatBytes(done), formatBytes(total), percent)
	}
	if done > 0 {
		return fmt.Sprintf("%s: %s", phase, formatBytes(done))
	}
	return phase
}

func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%dB", value)
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unitIndex := 0
	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}
	precision := 1
	if unitIndex >= 2 {
		precision = 2
	}
	return fmt.Sprintf("%.*f%s", precision, size, units[unitIndex])
}

func calcBitrateKbps(sizeBytes int64, durationMillis int64) float64 {
	if sizeBytes <= 0 || durationMillis <= 0 {
		return 0
	}
	seconds := float64(durationMillis) / 1000.0
	if seconds <= 0 {
		return 0
	}
	return (float64(sizeBytes) * 8.0) / (seconds * 1000.0)
}

func formatTelegramCaption(sizeBytes int64, bitrateKbps float64, format string) string {
	sizeMB := float64(sizeBytes) / (1024.0 * 1024.0)
	if sizeMB < 0 {
		sizeMB = 0
	}
	if bitrateKbps < 0 {
		bitrateKbps = 0
	}
	tag := normalizeTelegramFormat(format)
	if tag == "" {
		tag = telegramFormatFlac
	}
	return fmt.Sprintf("#AppleMusic #%s 文件大小%.2fMB %.2fkbps\nvia @ultimateapplemusicdownloaderbot", tag, sizeMB, bitrateKbps)
}

func extractInlineTrackID(query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "/songid") {
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			return strings.TrimSpace(fields[1])
		}
		return ""
	}
	if strings.HasPrefix(lower, "songid") {
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			return strings.TrimSpace(fields[1])
		}
		return ""
	}
	if strings.HasPrefix(lower, "song:") {
		return strings.TrimSpace(trimmed[5:])
	}
	return strings.TrimSpace(trimmed)
}

func findCoverFile(dir string) string {
	candidates := []string{
		"cover.jpg",
		"cover.png",
		"folder.jpg",
		"folder.png",
	}
	for _, name := range candidates {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func makeTelegramThumb(coverPath string) (string, error) {
	tmp, err := os.CreateTemp("", "amdl-thumb-*.jpg")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return "", err
	}
	args := []string{
		"-y", "-i", coverPath,
		"-vf", "scale=320:320:force_original_aspect_ratio=decrease",
		"-frames:v", "1",
		"-q:v", "5",
		tmpPath,
	}
	cmd := exec.Command(Config.FFmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("ffmpeg thumb failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if info, err := os.Stat(tmpPath); err == nil && info.Size() > 200*1024 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("thumb too large")
	}
	return tmpPath, nil
}

func (b *TelegramBot) compressFlacToSize(srcPath string, maxBytes int64) (string, error) {
	outPath, err := makeTempFlacPath()
	if err != nil {
		return "", err
	}
	coverPath := findCoverFile(filepath.Dir(srcPath))
	if err := runFlacCompress(srcPath, outPath, 0, 0, false, coverPath); err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	info, err := os.Stat(outPath)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if info.Size() <= maxBytes {
		return outPath, nil
	}

	duration, err := getAudioDurationSeconds(srcPath)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if duration <= 0 {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("invalid duration for %s", filepath.Base(srcPath))
	}

	targetBitsPerSec := (float64(maxBytes) * 8.0 / duration) * 0.95
	sampleRate, channels := chooseResamplePlan(targetBitsPerSec)
	if err := runFlacCompress(srcPath, outPath, sampleRate, channels, true, coverPath); err != nil {
		_ = os.Remove(outPath)
		return "", err
	}

	info, err = os.Stat(outPath)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if info.Size() > maxBytes {
		return "", fmt.Errorf("cannot compress below %dMB", maxBytes/1024/1024)
	}
	return outPath, nil
}

func runFlacCompress(srcPath, outPath string, sampleRate int, channels int, force16 bool, coverPath string) error {
	args := []string{"-y", "-i", srcPath}
	if coverPath != "" {
		args = append(args, "-i", coverPath)
		args = append(args,
			"-map", "0:a",
			"-map", "1:v",
			"-c:v", "mjpeg",
			"-disposition:v", "attached_pic",
		)
	} else {
		args = append(args, "-map", "0:a", "-map", "0:v?")
	}
	args = append(args,
		"-c:a", "flac",
		"-compression_level", "12",
	)
	if force16 {
		args = append(args, "-sample_fmt", "s16")
	}
	if sampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(sampleRate))
	}
	if channels > 0 {
		args = append(args, "-ac", strconv.Itoa(channels))
	}
	args = append(args, outPath)
	cmd := exec.Command(Config.FFmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg compress failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func chooseResamplePlan(targetBitsPerSec float64) (int, int) {
	channels := 2
	targetRate := targetBitsPerSec / float64(16*channels)
	if targetRate < 12000 {
		channels = 1
		targetRate = targetBitsPerSec / float64(16*channels)
	}
	return pickSampleRate(targetRate), channels
}

func pickSampleRate(target float64) int {
	rates := []int{48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000}
	for _, rate := range rates {
		if float64(rate) <= target {
			return rate
		}
	}
	return rates[len(rates)-1]
}

func makeTempFlacPath() (string, error) {
	tmp, err := os.CreateTemp("", "amdl-*.flac")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func getAudioDurationSeconds(path string) (float64, error) {
	if _, err := exec.LookPath("ffprobe"); err == nil {
		cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
		out, err := cmd.Output()
		if err == nil {
			value := strings.TrimSpace(string(out))
			if value != "" {
				if secs, err := strconv.ParseFloat(value, 64); err == nil && secs > 0 {
					return secs, nil
				}
			}
		}
	}

	cmd := exec.Command(Config.FFmpegPath, "-i", path)
	out, _ := cmd.CombinedOutput()
	re := regexp.MustCompile(`Duration:\s+(\d+):(\d+):(\d+(?:\.\d+)?)`)
	match := re.FindStringSubmatch(string(out))
	if len(match) != 4 {
		return 0, fmt.Errorf("failed to read duration from ffmpeg output")
	}
	hours, _ := strconv.ParseFloat(match[1], 64)
	minutes, _ := strconv.ParseFloat(match[2], 64)
	seconds, _ := strconv.ParseFloat(match[3], 64)
	return hours*3600 + minutes*60 + seconds, nil
}

func (b *TelegramBot) sendMessage(chatID int64, text string, markup any) error {
	return b.sendMessageWithReply(chatID, text, markup, 0)
}

func (b *TelegramBot) sendMessageWithReply(chatID int64, text string, markup any, replyToID int) error {
	_, err := b.sendMessageWithReplyReturn(chatID, text, markup, replyToID)
	return err
}

func (b *TelegramBot) sendMessageWithReplyReturn(chatID int64, text string, markup any, replyToID int) (int, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	if replyToID > 0 {
		payload["reply_to_message_id"] = replyToID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", b.apiURL("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("telegram sendMessage failed: %s", resp.Status)
	}
	apiResp := sendMessageResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return 0, err
	}
	if !apiResp.OK {
		return 0, fmt.Errorf("telegram sendMessage error: %s", apiResp.Description)
	}
	return apiResp.Result.MessageID, nil
}

func (b *TelegramBot) sendAudioByFileID(chatID int64, entry CachedAudio, replyToID int, trackID string) error {
	entry = b.enrichCachedAudio(trackID, entry)
	sizeBytes := entry.SizeBytes
	if sizeBytes <= 0 {
		sizeBytes = entry.FileSize
	}
	bitrateKbps := entry.BitrateKbps
	format := normalizeTelegramFormat(entry.Format)
	if format == "" {
		format = telegramFormatFlac
	}
	caption := formatTelegramCaption(sizeBytes, bitrateKbps, format)
	payload := map[string]any{
		"chat_id": chatID,
		"audio":   entry.FileID,
		"caption": caption,
	}
	if entry.Title != "" {
		payload["title"] = entry.Title
	}
	if entry.Performer != "" {
		payload["performer"] = entry.Performer
	}
	if replyToID > 0 {
		payload["reply_to_message_id"] = replyToID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("sendAudio"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendAudio failed: %s", strings.TrimSpace(string(responseBody)))
	}
	apiResp := sendAudioResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		return fmt.Errorf("telegram sendAudio error: %s", apiResp.Description)
	}
	return nil
}

func (b *TelegramBot) answerCallbackQuery(callbackID string) error {
	if callbackID == "" {
		return nil
	}
	payload := map[string]any{
		"callback_query_id": callbackID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("answerCallbackQuery"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *TelegramBot) answerInlineQuery(inlineQueryID string, results any, personal bool) error {
	if inlineQueryID == "" {
		return nil
	}
	payload := map[string]any{
		"inline_query_id": inlineQueryID,
		"results":         results,
		"is_personal":     personal,
		"cache_time":      0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("answerInlineQuery"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *TelegramBot) editMessageText(chatID int64, messageID int, text string, markup any) error {
	if messageID == 0 {
		return nil
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("editMessageText"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		apiResp := apiResponse{}
		if err := json.Unmarshal(responseBody, &apiResp); err == nil {
			if strings.Contains(apiResp.Description, "message is not modified") {
				return nil
			}
			if apiResp.Description != "" {
				return fmt.Errorf("telegram editMessageText error: %s", apiResp.Description)
			}
		}
		return fmt.Errorf("telegram editMessageText failed: %s", strings.TrimSpace(string(responseBody)))
	}
	apiResp := apiResponse{}
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return err
	}
	if !apiResp.OK {
		if strings.Contains(apiResp.Description, "message is not modified") {
			return nil
		}
		return fmt.Errorf("telegram editMessageText error: %s", apiResp.Description)
	}
	return nil
}

func (b *TelegramBot) deleteMessage(chatID int64, messageID int) error {
	if messageID == 0 {
		return nil
	}
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", b.apiURL("deleteMessage"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (b *TelegramBot) getUpdates(offset int) ([]Update, error) {
	req, err := http.NewRequest("GET", b.apiURL("getUpdates"), nil)
	if err != nil {
		return nil, err
	}
	query := req.URL.Query()
	query.Set("timeout", "30")
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	req.URL.RawQuery = query.Encode()
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getUpdates failed: %s", resp.Status)
	}
	var data getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if !data.OK {
		return nil, fmt.Errorf("getUpdates error: %s", data.Description)
	}
	return data.Result, nil
}

func (b *TelegramBot) apiURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", b.apiBase, b.token, method)
}

func (b *TelegramBot) isAllowedChat(chatID int64) bool {
	if len(b.allowedChats) == 0 {
		return true
	}
	return b.allowedChats[chatID]
}

func (b *TelegramBot) setPending(chatID int64, kind string, query string, offset int, items []apputils.SearchResultItem, hasNext bool, replyToID int, resultsMessageID int, title string) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	b.pending[chatID] = &PendingSelection{
		Kind:             kind,
		Query:            query,
		Title:            title,
		Offset:           offset,
		HasNext:          hasNext,
		Items:            items,
		CreatedAt:        time.Now(),
		ReplyToMessageID: replyToID,
		ResultsMessageID: resultsMessageID,
	}
}

func (b *TelegramBot) getPending(chatID int64) (*PendingSelection, bool) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	pending, ok := b.pending[chatID]
	return pending, ok
}

func (b *TelegramBot) clearPending(chatID int64) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	delete(b.pending, chatID)
}

func parseCommand(text string) (string, []string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", nil, false
	}
	cmd := strings.TrimPrefix(parts[0], "/")
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}
	return strings.ToLower(cmd), parts[1:], true
}

func buildInlineKeyboard(count int, hasPrev bool, hasNext bool) InlineKeyboardMarkup {
	rowSize := 4
	rows := [][]InlineKeyboardButton{}
	row := []InlineKeyboardButton{}
	for i := 1; i <= count; i++ {
		row = append(row, InlineKeyboardButton{
			Text:         strconv.Itoa(i),
			CallbackData: fmt.Sprintf("sel:%d", i),
		})
		if len(row) == rowSize {
			rows = append(rows, row)
			row = []InlineKeyboardButton{}
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	navRow := []InlineKeyboardButton{}
	if hasPrev {
		navRow = append(navRow, InlineKeyboardButton{
			Text:         "Prev",
			CallbackData: "page:-1",
		})
	}
	if hasNext {
		navRow = append(navRow, InlineKeyboardButton{
			Text:         "Next",
			CallbackData: "page:1",
		})
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}
	return InlineKeyboardMarkup{
		InlineKeyboard: rows,
	}
}

func buildSettingsKeyboard(current string) InlineKeyboardMarkup {
	current = normalizeTelegramFormat(current)
	if current == "" {
		current = defaultTelegramFormat
	}
	alacText := "ALAC"
	flacText := "FLAC"
	if current == telegramFormatAlac {
		alacText = "ALAC (current)"
	} else if current == telegramFormatFlac {
		flacText = "FLAC (current)"
	}
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: alacText, CallbackData: "setting:alac"},
				{Text: flacText, CallbackData: "setting:flac"},
			},
		},
	}
}

func botHelpText() string {
	return strings.TrimSpace(`
Commands:
/search_song <keywords>   search for songs
/search_album <keywords>  search for albums
/search_artist <keywords> search for artists
/search <type> <keywords> unified search (type: song|album|artist)
/songid <id>              download a song by ID
/albumid <id>             download an album by ID
/id <song|album> <id>     download by ID
/settings [alac|flac]     set download format (default: alac)
`)
}
