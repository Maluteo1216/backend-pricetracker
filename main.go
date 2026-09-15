package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
)

var db *sql.DB

// Estructura que viene desde la API de CheapShark
type CheapSharkDeal struct {
	Title       string `json:"title"`
	SalePrice   string `json:"salePrice"`
	NormalPrice string `json:"normalPrice"`
	Savings     string `json:"savings"`
	StoreID     string `json:"storeID"`
	DealID      string `json:"dealID"`
	Thumb       string `json:"thumb"`
}

type GameCreateRequest struct {
	Title string `json:"title"`
	Slug  string `json:"slug"`
}

type PriceUpdateRequest struct {
	StoreProductID int     `json:"store_product_id"`
	CurrentPrice   float64 `json:"current_price"`
	RegularPrice   float64 `json:"regular_price"`
}

func main() {
	var err error
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Println("Advertencia: No se encontró DATABASE_URL")
	}

	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	http.HandleFunc("/sync", corsMiddleware(handleSyncCheapShark))
	http.HandleFunc("/games", corsMiddleware(handleGames))
	http.HandleFunc("/prices", corsMiddleware(handlePrices))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Println("Servidor iniciado en puerto " + port)
	http.ListenAndServe(":"+port, nil)
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

// 1. CONEXIÓN A CHEAPSHARK API: Trae juegos reales y los guarda en PostgreSQL
func handleSyncCheapShark(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	// Consultar ofertas reales desde CheapShark API
	resp, err := http.Get("https://www.cheapshark.com/api/1.0/deals?pageSize=10")
	if err != nil {
		http.Error(w, "Error consultando CheapShark API", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var deals []CheapSharkDeal
	json.NewDecoder(resp.Body).Decode(&deals)

	insertados := 0
	for _, deal := range deals {
		slug := strings.ToLower(strings.ReplaceAll(deal.Title, " ", "-"))
		
		// Insertar o recuperar el juego en la tabla games
		var gameID int
		err := db.QueryRow("INSERT INTO games (title, slug, cover_image_url) VALUES ($1, $2, $3) ON CONFLICT (slug) DO UPDATE SET title=EXCLUDED.title RETURNING id;",
			deal.Title, slug, deal.Thumb).Scan(&gameID)
		if err != nil {
			continue
		}

		storeID, _ := strconv.Atoi(deal.StoreID)
		// Verificar que la tienda exista en nuestra DB
		var existStore int
		db.QueryRow("SELECT id FROM stores WHERE id = $1", storeID).Scan(&existStore)
		if existStore == 0 {
			storeID = 1 // Por defecto asigna Steam si no coincide
		}

		// Insertar producto por tienda
		var storeProductID int
		productURL := "https://www.cheapshark.com/redirect?dealID=" + deal.DealID
		err = db.QueryRow("INSERT INTO store_products (game_id, store_id, external_store_id, product_url) VALUES ($1, $2, $3, $4) ON CONFLICT (game_id, store_id) DO UPDATE SET product_url=EXCLUDED.product_url RETURNING id;",
			gameID, storeID, deal.DealID, productURL).Scan(&storeProductID)
		if err != nil {
			continue
		}

		// Convertir precios
		regPrice, _ := strconv.ParseFloat(deal.NormalPrice, 64)
		curPrice, _ := strconv.ParseFloat(deal.SalePrice, 64)
		discount, _ := strconv.ParseFloat(deal.Savings, 64)
		isOnSale := curPrice < regPrice

		// Insertar precio actual
		_, err = db.Exec(`
			INSERT INTO current_prices (store_product_id, regular_price, current_price, discount_percentage, is_on_sale)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (store_product_id) DO UPDATE 
			SET current_price = EXCLUDED.current_price, regular_price = EXCLUDED.regular_price, discount_percentage = EXCLUDED.discount_percentage, last_updated = CURRENT_TIMESTAMP;
		`, storeProductID, regPrice, curPrice, discount, isOnSale)

		if err == nil {
			insertados++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": fmt.Sprintf("¡Éxito! Se sincronizaron %d juegos reales desde CheapShark", insertados),
	})
}

// 2. READ, CREATE, DELETE para /games
func handleGames(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "GET" {
		// READ: Listar juegos
		rows, err := db.Query(`
			SELECT 
				g.id AS game_id,
				g.title,
				g.slug,
				COALESCE(s.name, 'Sin Tienda') AS store_name,
				COALESCE(cp.regular_price, 0) AS regular_price,
				COALESCE(cp.current_price, 0) AS current_price,
				COALESCE(cp.discount_percentage, 0) AS discount_percentage,
				COALESCE(sp.id, 0) AS store_product_id
			FROM games g
			LEFT JOIN store_products sp ON g.id = sp.game_id
			LEFT JOIN stores s ON sp.store_id = s.id
			LEFT JOIN current_prices cp ON sp.id = cp.store_product_id
			ORDER BY g.id DESC;
		`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var lista []map[string]interface{}
		for rows.Next() {
			var gameID, storeProductID int
			var title, slug, storeName string
			var regPrice, curPrice, discount float64

			rows.Scan(&gameID, &title, &slug, &storeName, &regPrice, &curPrice, &discount, &storeProductID)

			lista = append(lista, map[string]interface{}{
				"game_id":             gameID,
				"title":               title,
				"slug":                slug,
				"store_name":          storeName,
				"regular_price":       regPrice,
				"current_price":       curPrice,
				"discount_percentage": discount,
				"store_product_id":    storeProductID,
			})
		}
		json.NewEncoder(w).Encode(lista)

	} else if r.Method == "POST" {
		// CREATE: Crear juego manualmente
		var req GameCreateRequest
		json.NewDecoder(r.Body).Decode(&req)

		var newID int
		err := db.QueryRow("INSERT INTO games (title, slug) VALUES ($1, $2) RETURNING id;", req.Title, req.Slug).Scan(&newID)
		if err != nil {
			http.Error(w, "Error creando juego: "+err.Error(), 400)
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Juego creado manualmente con éxito",
			"game_id": newID,
		})

	} else if r.Method == "DELETE" {
		// DELETE: Eliminar juego
		gameID := r.URL.Query().Get("id")
		if gameID == "" {
			http.Error(w, "Falta el parámetro id", 400)
			return
		}

		_, err := db.Exec("DELETE FROM games WHERE id = $1;", gameID)
		if err != nil {
			http.Error(w, "Error al eliminar: "+err.Error(), 400)
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Juego eliminado correctamente",
		})
	}
}

// 3. UPDATE para /prices
func handlePrices(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		var req PriceUpdateRequest
		json.NewDecoder(r.Body).Decode(&req)

		discount := ((req.RegularPrice - req.CurrentPrice) / req.RegularPrice) * 100
		isOnSale := req.CurrentPrice < req.RegularPrice

		_, err := db.Exec(`
			UPDATE current_prices 
			SET current_price = $1, regular_price = $2, discount_percentage = $3, is_on_sale = $4, last_updated = CURRENT_TIMESTAMP
			WHERE store_product_id = $5;
		`, req.CurrentPrice, req.RegularPrice, discount, isOnSale, req.StoreProductID)

		if err != nil {
			http.Error(w, "Error actualizando precio: "+err.Error(), 400)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Precio actualizado correctamente en la base de datos",
		})
	}
}
