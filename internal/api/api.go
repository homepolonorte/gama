package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/arturhvcpn/ws-go/internal/store/pgstore"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	handlerApi := apiHandler{
		q: q,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	router := chi.NewRouter()

	router.Use(middleware.RequestID, middleware.RealIP, middleware.Logger, middleware.Recoverer)

	router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	router.Get("/subscribe/{room_id}", handlerApi.handleSubscribe)

	router.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Get("/", handlerApi.handleGetRooms)
			r.Post("/", handlerApi.handleCreateRoom)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Get("/", handlerApi.handleGetRoomMessages)
				r.Post("/", handlerApi.handleCreateRoomMessage)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", handlerApi.handleGetRoomMessage)
					r.Patch("/react", handlerApi.handleReactToMessage)
					r.Delete("/react", handlerApi.handleRemoveReactFromMessage)
					r.Patch("/answer", handlerApi.handleMarkMessageAsAnswered)
				})
			})
		},
		)
	})

	handlerApi.r = router

	return handlerApi
}

const (
	KindCreatedMessage = "message_created"
	KindReact          = "message_reacted"
	KindAnswer         = "message_answered"
)

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type Message struct {
	Kind    string `json:"kind"`
	Value   any    `json:"value"`
	RoomdId string `json:"-"`
}

func (h apiHandler) notifyClients(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs, ok := h.subscribers[msg.RoomdId]

	if !ok || len(subs) == 0 {
		return
	}

	for conn, cancel := range subs {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message", "error", err)
			cancel()
		}
	}
}

func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	rawRoomId := chi.URLParam(r, "room_id")

	roomID, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(w, "Invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}

		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	con, err := h.upgrader.Upgrade(w, r, nil)

	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(w, "Failed to upgrade connection", http.StatusBadRequest)
		return
	}

	defer con.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mu.Lock()

	if _, ok := h.subscribers[rawRoomId]; !ok {
		h.subscribers[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("new connection", "room_id", rawRoomId, "remote_addr", r.RemoteAddr)

	h.subscribers[rawRoomId][con] = cancel

	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()

	delete(h.subscribers[rawRoomId], con)

	h.mu.Unlock()
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	roomId, err := h.q.InsertRoom(r.Context(), body.Theme)

	if err != nil {
		slog.Error("failed to insert room", "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, err := json.Marshal(response{ID: roomId.String()})

	if err != nil {
		slog.Error("failed to marshal response", "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomId := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(w, "Invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}

		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	msgId, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{
		RoomID:  roomID,
		Message: body.Message,
	})

	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, err := json.Marshal(response{ID: msgId.String()})

	if err != nil {
		slog.Error("failed to marshal response", "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

	go h.notifyClients(Message{
		Kind:    KindCreatedMessage,
		RoomdId: rawRoomId,
		Value: MessageMessageCreated{
			ID:      msgId.String(),
			Message: body.Message,
		},
	})
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {}

func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {}

func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {}

func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {}
