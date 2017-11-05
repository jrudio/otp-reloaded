package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"flag"

	"strconv"

	"github.com/asdine/storm"
	"github.com/gorilla/mux"
	"github.com/jrudio/go-plex-client"
	"github.com/rs/xid"
)

const databaseFileName = "onetimeplex.db"

var cwd string

type plexSearchResults struct {
	Title     string `json:"title"`
	Year      string `json:"year"`
	MediaID   string `json:"mediaID"`
	MediaType string `json:"type"`
}

type plexFriend struct {
	ID              string `json:"id"`
	Username        string `json:"username"`
	ServerName      string `json:"serverName"`
	ServerID        string `json:"serverID"`
	ServerMachineID string `json:"serverMachineID"`
}

type clientResponse struct {
	Result interface{} `json:"result"`
	Err    string      `json:"error"`
}

type restrictedUser struct {
	ID              string `storm:"id" json:"id"`
	Name            string `json:"plexUsername"`
	PlexUserID      string `storm:"unique" json:"plexUserID"`
	AssignedMediaID string `json:"assignedMediaID"`
	Title           string `json:"title"`
}

type usersPayload struct {
	Result []restrictedUser `json:"result"`
}

func (u usersPayload) toBytes() ([]byte, error) {
	return json.Marshal(u)
}

func (r restrictedUser) toBytes() ([]byte, error) {
	return json.Marshal(r)
}

func (c clientResponse) Write(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "POST GET")

	response, err := json.Marshal(&c)

	if err != nil {
		return err
	}

	_, err = w.Write(response)

	return err
}

// OnPlay reads and reacts to the webhook sent from Plex
func OnPlay(db *storm.DB, plexConnection *plex.Plex) func(wh plex.Webhook) {
	return func(wh plex.Webhook) {
		userID := wh.Account.ID
		username := wh.Account.Title
		title := wh.Metadata.Title
		mediaID := wh.Metadata.RatingKey

		fmt.Printf("%s (%d) has started playing %s (%s)\n", username, userID, title, mediaID)

		// is this a user we need to check?
		var user restrictedUser

		if err := db.One("PlexUserID", userID, &user); err != nil {
			// user not in database so we don't care about them
			fmt.Printf("user %s (%d) is not in database\n", username, userID)
			return
		}

		if user.AssignedMediaID == mediaID {
			// user is watching what they were assigned
			fmt.Printf("user %s (%d) is watching %s which is ok\n", username, userID, mediaID)
			return
		}

		// Obtain session id
		//
		// We will assume the plexConnection is the server that sent this webhook
		sessions, err := plexConnection.GetSessions()

		if err != nil {
			fmt.Printf("not terminating user: %s (%d) \n\tfailed to grab sessions from plex server: %v\n", username, userID, err)
			return
		}

		var sessionID string

		for _, session := range sessions.Video {
			if session.User.ID != userID {
				continue
			}

			sessionID = session.Session.ID
			break
		}

		// kill session
		fmt.Printf("Terminating %s (%d)'s session as they are not supposed to be watching %s (%s)\n", username, userID, title, mediaID)
		plexConnection.TerminateSession(sessionID, "One Time Plex: You are not allowed to watch that")
		// fmt.Printf("%d is now playing: %s (%s)\n", userID, title, mediaID)
	}
}

// OnStop will stop monitoring the user and unshare the Plex library
func OnStop(db *storm.DB, plexConnection *plex.Plex) func(wh plex.Webhook) {
	return func(wh plex.Webhook) {
		// remove from our database
		username := wh.Account.Title
		userID := wh.Account.ID

		var user restrictedUser

		if err := db.One("PlexUserID", userID, &user); err != nil {
			// user not in database, don't care
			fmt.Printf("user %s (%d) is not in database\n", username, userID)
			return
		}

		if err := db.DeleteStruct(&user); err != nil {
			fmt.Printf("user %s (%d) removal failed\n", username, userID)
			return
		}

		// unshare the Plex library
		_, err := plexConnection.RemoveFriend(strconv.Itoa(userID))

		if err != nil {
			fmt.Printf("failed to unshare library with %s (%d)\n\tplease remove them manually\n", username, userID)
			return
		}

		fmt.Printf("%s (%d) has finished viewing: %s", username, userID, wh.Metadata.Title)
	}
}

// AddUser adds a user that needs to be monitored
func AddUser(db *storm.DB, plexConnection *plex.Plex) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var user restrictedUser
		var resp clientResponse

		// check for required parameters
		defer r.Body.Close()

		if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
			resp.Err = fmt.Sprintf("failed to decode json body: %v", err)
			resp.Write(w)
			return
		}

		if user.PlexUserID == "" || user.AssignedMediaID == "" {
			resp.Err = "missing 'plexuserid' and/or 'mediaID' in the post form body"
			resp.Write(w)
			return
		}

		user.ID = xid.New().String()
		// user.Name = plexUsername
		// user.PlexUserID = plexUserID
		// user.AssignedMediaID = mediaID

		metadata, err := plexConnection.GetMetadata(user.AssignedMediaID)

		if err != nil {
			resp.Err = fmt.Sprintf("failed to fetch title for media id %s: %v", user.AssignedMediaID, err)
			resp.Write(w)
			return
		}

		metadataLen := metadata.MediaContainer.Size

		if metadataLen > 0 {
			data := metadata.MediaContainer.Metadata[0]

			var title string

			// combine show name, season, and episode name if type == episode
			if data.Type == "episode" {
				title = data.GrandparentTitle + ": " + data.ParentTitle + " - " + data.Title
			} else {
				// type is movie just need the title
				title = metadata.MediaContainer.Metadata[0].Title
			}

			user.Title = title
		}

		if err := db.Save(&user); err != nil {
			resp.Err = fmt.Sprintf("failed to save user: %v\n", err)
			resp.Write(w)
			return
		}

		resp.Result = user

		resp.Write(w)
	}
}

// GetAllUsers returns all monitored users
func GetAllUsers(db *storm.DB) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var users []restrictedUser
		var resp clientResponse

		if err := db.All(&users); err != nil {
			resp.Err = fmt.Sprintf("failed to retrieve users: %v\n", err)
			resp.Write(w)
			return
		}

		resp.Result = users
		resp.Write(w)
	}
}

func filterSearchResults(results plex.SearchResults) []plexSearchResults {
	var newResults []plexSearchResults

	count := results.MediaContainer.Size

	if count == 0 {
		return newResults
	}

	for _, r := range results.MediaContainer.Metadata {
		filtered := plexSearchResults{
			MediaType: r.Type,
			MediaID:   r.RatingKey,
			Title:     r.Title,
			Year:      "N/A", // default to n/a if we can't convert to string
		}

		if year := strconv.FormatInt(r.Year, 10); year != "" {
			filtered.Year = year
		}

		newResults = append(newResults, filtered)
	}

	return newResults
}

// SearchPlex is an endpoint that will search your Plex Media Server for media
func SearchPlex(plexConnection *plex.Plex) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {

		var resp clientResponse

		searchQuery := r.URL.Query().Get("title")

		if searchQuery == "" {
			resp.Err = "missing search query: 'title'"
			resp.Write(w)
			return
		}

		results, err := plexConnection.Search(searchQuery)

		if err != nil {
			resp.Err = fmt.Sprintf("search on plex media server failed: %v", err)
			resp.Write(w)
			return
		}

		// filter results with relevant information
		resp.Result = filterSearchResults(results)

		resp.Write(w)
	}
}

// GetPlexFriends will return an array of usernames and ids that are friends with associated plex token
func GetPlexFriends(plexConnection *plex.Plex) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var resp clientResponse

		friends, err := plexConnection.GetFriends()

		if err != nil {
			resp.Err = fmt.Sprintf("failed to fetch friends from plex: %v", err)
			resp.Write(w)
			return
		}

		var friendsFiltered []plexFriend

		for _, friend := range friends {
			filteredFriend := plexFriend{
				ID:              strconv.Itoa(friend.ID),
				Username:        friend.Username,
				ServerID:        friend.Server.ID,
				ServerMachineID: friend.Server.MachineIdentifier,
				ServerName:      friend.Server.Name,
			}

			friendsFiltered = append(friendsFiltered, filteredFriend)
		}

		resp.Result = friendsFiltered
		resp.Write(w)
	}
}

// GetMetadataFromPlex fetches metadata of media from plex
func GetMetadataFromPlex(plexConnection *plex.Plex) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var resp clientResponse

		mediaID := r.URL.Query().Get("mediaid")

		if mediaID == "" {
			resp.Err = "missing id from query: 'mediaID'"
			w.WriteHeader(http.StatusBadRequest)
			resp.Write(w)
			return
		}

		metadata, err := plexConnection.GetMetadataChildren(mediaID)

		if err != nil {
			resp.Err = fmt.Sprintf("failed to grab metadata from plex: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			resp.Write(w)
			return
		}

		var results []plexSearchResults

		for _, child := range metadata.MediaContainer.Metadata {
			newResult := plexSearchResults{
				Title:     child.ParentTitle + " - " + child.Title,
				MediaID:   child.RatingKey,
				MediaType: child.Type,
			}

			results = append(results, newResult)
		}

		resp.Result = results

		resp.Write(w)
	}
}

func main() {
	// grab optional params
	writeDefaultConfigFile := flag.Bool("write", false, "create a default config file where ever the -config flag points to")
	configFilePath := flag.String("config", "./", "path to your one time plex config file. File should be labeled: "+configFilename)
	dbFilePath := flag.String("db", "./", "path to your one time plex database file. File should be labeled: "+databaseFileName)

	flag.Parse()

	// get current working directory and set as the config-functions rely on this
	cwd, err := os.Getwd()

	if err != nil {
		fmt.Printf("failed to get current working directory: %v\n", err)
		os.Exit(2)
	}

	// capture write default config flag
	if *writeDefaultConfigFile {
		if _, err := WriteDefaultConfig(*configFilePath); err != nil {
			fmt.Printf("config write failed: %v\n", err)
			os.Exit(2)
		}

		// write was successful
		fmt.Println("sucessfully wrote configuration file")
		os.Exit(1)
	}

	// load settings
	config, err := ReadConfig(*configFilePath)

	if err != nil {
		fmt.Printf("failed to read config: %v\n", err)
		os.Exit(2)
	}

	if *dbFilePath == "./" {
		*dbFilePath = filepath.Join(cwd, databaseFileName)
	}

	// connect to database
	db, err := storm.Open(*dbFilePath)

	if err != nil {
		fmt.Printf("database connection failed: %v\n", err)
		os.Exit(2)
	}

	// set up plex connection
	plexConnection, err := plex.New(config.PlexHost, config.PlexToken)

	if err != nil {
		fmt.Printf("connection to plex media server failed: %v\n", err)
		os.Exit(2)
	}

	router := mux.NewRouter()

	wh := plex.NewWebhook()

	wh.OnPlay(OnPlay(db, plexConnection))

	wh.OnStop(OnStop(db, plexConnection))

	router.HandleFunc("/webhook", wh.Handler)

	apiRouter := router.PathPrefix("/api").Subrouter()

	// add new restricted user
	apiRouter.HandleFunc("/users/add", AddUser(db, plexConnection)).Methods("POST")

	// list restricted users
	apiRouter.HandleFunc("/users", GetAllUsers(db)).Methods("GET")

	// search media on plex
	apiRouter.HandleFunc("/search", SearchPlex(plexConnection)).Methods("GET")

	// get plex friends
	apiRouter.HandleFunc("/friends", GetPlexFriends(plexConnection)).Methods("GET")

	// get child data from plex
	apiRouter.HandleFunc("/metadata", GetMetadataFromPlex(plexConnection)).Methods("GET")

	fmt.Printf("serving one time plex on %s\n", config.Host)

	if err := http.ListenAndServe(config.Host, router); err != nil {
		fmt.Printf("server failed to start: %v\n", err)
		os.Exit(2)
	}
}
