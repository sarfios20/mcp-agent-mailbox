package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Límites del servidor HTTP. No dependen del entorno: se cambian aquí.
const (
	// maxRequestBodyBytes es el tamaño máximo de una petición MCP; por encima responde 413.
	maxRequestBodyBytes int64 = 1 << 20
	// readHeaderTimeout acota cuánto puede tardar un cliente en mandar las cabeceras.
	readHeaderTimeout = 5 * time.Second
	// readTimeout acota la lectura de la petición completa, cuerpo incluido.
	readTimeout = 30 * time.Second
	// writeTimeout acota cuánto puede tardar en escribirse la respuesta.
	writeTimeout = 60 * time.Second
	// idleTimeout cierra las conexiones keep-alive que llevan este tiempo sin uso.
	idleTimeout = 120 * time.Second
)

// Config reúne todos los ajustes de arranque del servidor.
type Config struct {
	// DBPath es la ruta del fichero SQLite configurada con BUZON_DB.
	DBPath string
	// TokensFile es la ruta del JSON configurada con BUZON_TOKENS_FILE.
	TokensFile string
	// Addr es el host:puerto de escucha configurado con BUZON_ADDR.
	Addr string
	// Tokens relaciona cada participante con su bearer token.
	Tokens map[string]string
}

// loadConfig lee el entorno y el fichero de tokens, y solo devuelve una
// configuración lista para abrir el buzón y arrancar el servidor HTTP.
func loadConfig() (Config, error) {
	dbPath, err := requiredEnv("BUZON_DB")
	if err != nil {
		return Config{}, err
	}
	tokensFile, err := requiredEnv("BUZON_TOKENS_FILE")
	if err != nil {
		return Config{}, err
	}
	addr, err := requiredEnv("BUZON_ADDR")
	if err != nil {
		return Config{}, err
	}
	if err := validateAddr(addr); err != nil {
		return Config{}, fmt.Errorf("BUZON_ADDR %q: %w", addr, err)
	}

	data, err := os.ReadFile(tokensFile)
	if err != nil {
		return Config{}, fmt.Errorf("BUZON_TOKENS_FILE %q: %w", tokensFile, err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return Config{}, fmt.Errorf("BUZON_TOKENS_FILE %q: JSON inválido: %w", tokensFile, err)
	}
	if len(tokens) < 2 {
		return Config{}, fmt.Errorf("BUZON_TOKENS_FILE %q: se requieren al menos dos participantes", tokensFile)
	}
	seenTokens := make(map[string]string, len(tokens))
	for name, token := range tokens {
		if strings.TrimSpace(name) == "" {
			return Config{}, fmt.Errorf("BUZON_TOKENS_FILE %q: nombre de participante vacío", tokensFile)
		}
		if strings.TrimSpace(token) == "" {
			return Config{}, fmt.Errorf("BUZON_TOKENS_FILE %q: token vacío para %q", tokensFile, name)
		}
		if previous, exists := seenTokens[token]; exists {
			return Config{}, fmt.Errorf("BUZON_TOKENS_FILE %q: token repetido para %q y %q", tokensFile, previous, name)
		}
		seenTokens[token] = name
	}

	return Config{DBPath: dbPath, TokensFile: tokensFile, Addr: addr, Tokens: tokens}, nil
}

func requiredEnv(name string) (string, error) {
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("falta %s o está vacía", name)
	}
	return value, nil
}

func validateAddr(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("debe tener formato host:puerto: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("el host está vacío")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("el puerto %q no es válido", portText)
	}
	return nil
}
