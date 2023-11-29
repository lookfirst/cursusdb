/*
* CursusDB
* Node
* ******************************************************************
* Copyright (C) 2023 CursusDB
*
* This program is free software: you can redistribute it and/or modify
* it under the terms of the GNU General Public License as published by
* the Free Software Foundation, either version 3 of the License, or
* (at your option) any later version.
*
* This program is distributed in the hope that it will be useful,
* but WITHOUT ANY WARRANTY; without even the implied warranty of
* MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
* GNU General Public License for more details.
*
* You should have received a copy of the GNU General Public License
* along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net"
	"net/textproto"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Curode is the CursusDB cluster nodestruct
type Curode struct {
	TCPAddr           *net.TCPAddr           // Cluster TCPAddr
	TCPListener       *net.TCPListener       // Cluster TCPListener
	Wg                *sync.WaitGroup        // Main node wait group
	SignalChannel     chan os.Signal         // Signal channel
	ConnectionQueue   map[string]*Connection // Hashmap of current connections
	ConnectionQueueMu *sync.RWMutex          // Connection queue mutex
	ConnectionChannel chan *Connection       // Connection channel for ConnectionEventWorker
	Data              Data                   // Node data
	Config            Config                 // Node config
	ContextCancel     context.CancelFunc     // For gracefully shutting down
	Context           context.Context        // Main looped go routine context.  This is for listeners, event loops and so forth
	TLSConfig         *tls.Config            // Node TLS config if TLS is true
}

// Config is the cluster config struct
type Config struct {
	TLSCert                string `yaml:"tls-cert"`                 // TLS cert path
	TLSKey                 string `yaml:"tls-key"`                  // TLS cert key
	Host                   string `yaml:"host"`                     // Node host i.e 0.0.0.0 usually
	TLS                    bool   `default:"false" yaml:"tls"`      // Use TLS?
	Port                   int    `yaml:"port"`                     // Node port
	Key                    string `yaml:"key"`                      // Key for a cluster to communicate with the node and also used to resting data.
	MaxMemory              uint64 `yaml:"max-memory"`               // Default 10240MB = 10 GB (1024 * 10)
	ConnectionQueueWorkers int    `yaml:"connection-queue-workers"` // Amount of go routines to listen to events.  The worker distributes connections iteratively
}

// Data is the node data struct
type Data struct {
	Map     map[string][]map[string]interface{} // Data hash map
	Writers map[string]*sync.RWMutex            // Collection writers
}

// Connection is a node tcp connection struct
type Connection struct {
	Conn net.Conn        // net Conn pointer
	Text *textproto.Conn // Connection writer and reader
}

// StartTCP_TLSListener start listening on TCP or TLS on provided port
func (curode *Curode) StartTCP_TLSListener() {
	var err error
	defer curode.Wg.Done() // defer go routine wait group done

	// Resolve the string address to a TCP address
	curode.TCPAddr, err = net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", curode.Config.Host, curode.Config.Port))

	if err != nil {
		fmt.Println("StartTCP_TLSListener():", err)
		curode.SignalChannel <- os.Interrupt
		return
	}

	// If node is configured for TLS
	if curode.Config.TLS {

		if curode.Config.TLSCert == "" || curode.Config.TLSKey == "" {
			fmt.Println("TCP_TLSListener():", "TLS cert and key missing.") // Log an error
			curode.SignalChannel <- os.Interrupt                           // Send interrupt to signal channel
			return
		}

		// Load provided key and cert
		cer, err := tls.LoadX509KeyPair(curode.Config.TLSCert, curode.Config.TLSKey)
		if err != nil {
			fmt.Println("TCP_TLSListener():", err.Error()) // Log an error
			curode.SignalChannel <- os.Interrupt           // Send interrupt to signal channel
			return                                         // close up go routine
		}

		// Set curode TLS config
		curode.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cer}}

	}

	// Start listening for TCP connections on the given address
	curode.TCPListener, err = net.ListenTCP("tcp", curode.TCPAddr)
	if err != nil {
		fmt.Println("StartTCP_TLSListener():", err)
		curode.SignalChannel <- os.Interrupt
		return
	}

	for {
		if curode.Context.Err() != nil {
			curode.TCPListener.Close()

			// Writing in memory data to file, encrypting data as well.
			curode.WriteToFile()

			close(curode.ConnectionChannel)

			for _, c := range curode.ConnectionQueue {
				c.Conn.Close()
			}

			return
		}
		curode.TCPListener.SetDeadline(time.Now().Add(time.Nanosecond * 1000000)) // 2000 connections a second
		conn, err := curode.TCPListener.Accept()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}

		// If TLS is set to true within config let's make the connection secure
		if curode.Config.TLS {
			conn = tls.Server(conn, curode.TLSConfig)
		}

		connection := &Connection{Conn: conn, Text: textproto.NewConn(conn)}

		// Expect Authentication: username\0password b64 encoded
		auth, err := connection.Text.ReadLine()
		if err != nil {
			connection.Text.PrintfLine("%d %s", 3, "Unable to read authentication header.")
			continue
		}

		if !strings.HasPrefix(auth, "Key:") {
			connection.Text.PrintfLine("Invalid key.  Node expecting cluster key.")
			continue
		} else {

			authSpl := strings.Split(auth, "Key:")

			if len(authSpl) != 2 {
				connection.Text.PrintfLine("Invalid key.  Node expecting cluster key.")
				continue
			}

			if curode.Config.Key != strings.TrimSpace(authSpl[1]) {
				connection.Text.PrintfLine("Invalid key.")
				continue
			}

			connection.Text.PrintfLine("0 Authentication successful.")

			// Authentication was a success, now we add connection to queue and start listening to events!
			curode.ConnectionQueueMu.Lock()
			curode.ConnectionQueue[conn.RemoteAddr().String()] = connection
			curode.ConnectionQueueMu.Unlock()

		}

	}
}

// CurrentMemoryUsage returns current memory usage in mb
func (curode *Curode) CurrentMemoryUsage() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return m.Alloc / 1024 / 1024
}

// Encrypt encrypts a temporary serialized .cdat serialized file with chacha
func (curode *Curode) Encrypt(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}

	totalLen := aead.NonceSize() + len(plaintext) + aead.Overhead()
	nonce := make([]byte, aead.NonceSize(), totalLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts .cdat file to temporary serialized data file to be read
func (curode *Curode) Decrypt(key, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}

	// Split nonce and ciphertext.
	nonce, ciphertext := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]

	return aead.Open(nil, nonce, ciphertext, nil)
}

// WriteToFile will write the current node data to a .cdat file encrypted with your node key.
func (curode *Curode) WriteToFile() {

	// Create temporary .cdat which is all serialized data.  An encryption is performed after the fact to not consume memory.
	fTmp, err := os.OpenFile(".cdat.tmp", os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0777)
	if err != nil {
		fmt.Println("WriteToFile():", err.Error())
		os.Exit(1)
	}

	e := gob.NewEncoder(fTmp)

	// Encoding the map
	err = e.Encode(curode.Data.Map)
	if err != nil {
		fmt.Println("WriteToFile():", err.Error())
		os.Exit(1)
	}

	fTmp.Close()

	// After serialization encrypt temp data file
	fTmp, err = os.OpenFile(".cdat.tmp", os.O_RDONLY, 0777)
	if err != nil {
		fmt.Println("WriteToFile():", err.Error())
		os.Exit(1)
	}

	//
	reader := bufio.NewReader(fTmp)
	buf := make([]byte, 1024)
	f, err := os.OpenFile(".cdat", os.O_TRUNC|os.O_CREATE|os.O_RDWR|os.O_APPEND, 0777)
	if err != nil {
		fmt.Println("WriteToFile():", err.Error())
		os.Exit(1)
	}
	defer f.Close()

	for {
		read, err := reader.Read(buf)

		if err != nil {
			if err != io.EOF {
				fmt.Println("WriteToFile():", err.Error())
				os.Exit(1)
			}
			break
		}

		if read > 0 {
			decodedKey, err := base64.StdEncoding.DecodeString(curode.Config.Key)
			if err != nil {
				fmt.Println("WriteToFile():", err.Error())
				os.Exit(1)
			}

			cipherblock, err := curode.Encrypt(decodedKey[:], buf[:read])
			if err != nil {
				fmt.Println("WriteToFile():", err.Error())
				os.Exit(1)
			}

			f.Write(cipherblock)
		}
	}

	os.Remove(".cdat.tmp")

	fmt.Println("WriteToFile(): Completed.")
}

// Update is a function to update the nodes data map
func (curode *Curode) Update(collection string, ks []interface{}, vs []interface{}, uks []interface{}, nvs []interface{}, vol int, skip int, oprs []interface{}, conditions []interface{}) []interface{} {

	var objects []*map[string]interface{}

	var conditionsMet uint64
	//The && operator updates documents if all the conditions are TRUE.
	//The || operator updates documents if any of the conditions are TRUE.

	for i, d := range curode.Data.Map[collection] {
		if ks == nil && vs == nil && oprs == nil {
			if skip != 0 {
				skip = skip - 1
				continue
			}

			if vol != -1 {
				if i-1 == vol-1 {
					break
				}
			}

			objects = append(objects, &curode.Data.Map[collection][i])
			continue
		} else {

			for m, k := range ks {

				if oprs[m] == "" {
					fmt.Sprintf("Query operator required.")
					return nil
				}

				if skip != 0 {
					skip = skip - 1
					continue
				}

				if vol != -1 {
					if len(objects) == vol {
						break
					}
				}

				vType := fmt.Sprintf("%T", vs[m])

				_, ok := d[k.(string)]
				if ok {

					if d[k.(string)] == nil {
						objects = append(objects, &curode.Data.Map[collection][i])
						continue
					}

					if reflect.TypeOf(d[k.(string)]).Kind() == reflect.Slice {
						for _, dd := range d[k.(string)].([]interface{}) {

							if len(objects) == vol {
								//return objects
								break
							}

							if reflect.TypeOf(dd).Kind() == reflect.Float64 {
								if vType == "int" {
									var interfaceI int = int(dd.(float64))

									if oprs[m] == "==" {
										if reflect.DeepEqual(interfaceI, vs[m]) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}
									} else if oprs[m] == "!=" {
										if !reflect.DeepEqual(interfaceI, vs[m]) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}
									} else if oprs[m] == ">" {
										if vType == "int" {
											if interfaceI > vs[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, &curode.Data.Map[collection][i])
												exists:
												})()
											}
										}
									} else if oprs[m] == "<" {
										if vType == "int" {
											if interfaceI < vs[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, &curode.Data.Map[collection][i])
												exists:
												})()
											}
										}
									} else if oprs[m] == ">=" {
										if vType == "int" {
											if interfaceI >= vs[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, &curode.Data.Map[collection][i])
												exists:
												})()
											}
										}
									} else if oprs[m] == "<=" {
										if vType == "int" {
											if interfaceI <= vs[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, &curode.Data.Map[collection][i])
												exists:
												})()
											}
										}
									}
								} else if vType == "float64" {
									var interfaceI float64 = dd.(float64)

									if oprs[m] == "==" {

										if bytes.Equal([]byte(fmt.Sprintf("%f", float64(interfaceI))), []byte(fmt.Sprintf("%f", float64(vs[m].(float64))))) {

											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}
									} else if oprs[m] == "!=" {
										if float64(interfaceI) != vs[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &d)
											exists:
											})()
										}
									} else if oprs[m] == ">" {
										if float64(interfaceI) > vs[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}

									} else if oprs[m] == "<" {
										if float64(interfaceI) < vs[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}

									} else if oprs[m] == ">=" {

										if float64(interfaceI) >= vs[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}

									} else if oprs[m] == "<=" {
										if float64(interfaceI) <= vs[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, &curode.Data.Map[collection][i])
											exists:
											})()
										}

									}
								}
							} else if reflect.TypeOf(dd).Kind() == reflect.Map {
								//for kkk, ddd := range dd.(map[string]interface{}) {
								//	// unimplemented
								//}
							} else {
								if oprs[m] == "==" {
									if reflect.DeepEqual(dd, vs[m]) {
										conditionsMet += 1
										(func() {
											for _, o := range objects {
												if reflect.DeepEqual(o, d) {
													goto exists
												}
											}
											objects = append(objects, &curode.Data.Map[collection][i])
										exists:
										})()
									}
								} else if oprs[m] == "!=" {
									if !reflect.DeepEqual(dd, vs[m]) {
										conditionsMet += 1
										(func() {
											for _, o := range objects {
												if reflect.DeepEqual(o, d) {
													goto exists
												}
											}
											objects = append(objects, &curode.Data.Map[collection][i])
										exists:
										})()
									}
								}
							}

						}
					} else if vType == "int" {
						var interfaceI int = int(d[k.(string)].(float64))

						if oprs[m] == "==" {
							if reflect.DeepEqual(interfaceI, vs[m]) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}
						} else if oprs[m] == "!=" {
							if !reflect.DeepEqual(interfaceI, vs[m]) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}
						} else if oprs[m] == ">" {
							if vType == "int" {
								if interfaceI > vs[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, &curode.Data.Map[collection][i])
									exists:
									})()
								}
							}
						} else if oprs[m] == "<" {
							if vType == "int" {
								if interfaceI < vs[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, &curode.Data.Map[collection][i])
									exists:
									})()
								}
							}
						} else if oprs[m] == ">=" {
							if vType == "int" {
								if interfaceI >= vs[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, &curode.Data.Map[collection][i])
									exists:
									})()
								}
							}
						} else if oprs[m] == "<=" {
							if vType == "int" {
								if interfaceI <= vs[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, &curode.Data.Map[collection][i])
									exists:
									})()
								}
							}
						}
					} else if vType == "float64" {
						var interfaceI float64 = d[k.(string)].(float64)

						if oprs[m] == "==" {

							if bytes.Equal([]byte(fmt.Sprintf("%f", float64(interfaceI))), []byte(fmt.Sprintf("%f", float64(vs[m].(float64))))) {

								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}
						} else if oprs[m] == "!=" {
							if float64(interfaceI) != vs[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}
						} else if oprs[m] == ">" {
							if float64(interfaceI) > vs[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}

						} else if oprs[m] == "<" {
							if float64(interfaceI) < vs[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}

						} else if oprs[m] == ">=" {

							if float64(interfaceI) >= vs[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}

						} else if oprs[m] == "<=" {
							if float64(interfaceI) <= vs[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}

						}
					} else {
						if oprs[m] == "==" {
							if reflect.DeepEqual(d[k.(string)], vs[m]) {
								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()

							}
						} else if oprs[m] == "!=" {
							if !reflect.DeepEqual(d[k.(string)], vs[m]) {
								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, &curode.Data.Map[collection][i])
								exists:
								})()
							}
						}

					}
				}
			}
		}

	}

	var updated []interface{}

	if slices.Contains(conditions, "&&") {
		var nullObjects []interface{}

		if uint64(len(conditions)) != conditionsMet {

			if !slices.Contains(conditions, "||") {
				return nullObjects
			} else if conditionsMet > 0 {
				for _, d := range objects {
					for m, _ := range uks {

						curode.Data.Writers[collection].Lock()
						ne := make(map[string]interface{})

						for kk, vv := range *d {
							ne[kk] = vv
						}

						ne[uks[m].(string)] = nvs[m]

						*d = ne
						updated = append(updated, *d)
						curode.Data.Writers[collection].Unlock()

					}

				}
			}
		}

	} else if conditionsMet > 0 {
		for _, d := range objects {
			for m, _ := range uks {

				curode.Data.Writers[collection].Lock()
				ne := make(map[string]interface{})

				for kk, vv := range *d {
					ne[kk] = vv
				}

				ne[uks[m].(string)] = nvs[m]

				*d = ne
				updated = append(updated, *d)
				curode.Data.Writers[collection].Unlock()

			}

		}
	}

	return updated
}

// Delete delete from node map
func (curode *Curode) Delete(collection string, ks interface{}, vs interface{}, vol int, skip int, oprs interface{}, lock bool, conditions []interface{}) []interface{} {

	var objects []uint64

	var conditionsMet uint64

	for i, d := range curode.Data.Map[collection] {
		if ks == nil && vs == nil && oprs == nil {
			if skip != 0 {
				skip = skip - 1
				continue
			}

			if vol != -1 {
				if i-1 == vol-1 {
					break
				}
			}
			objects = append(objects, uint64(i))

			continue
		} else {

			for m, k := range ks.([]interface{}) {

				if oprs.([]interface{})[m] == "" {
					fmt.Sprintf("Query operator required.")
					return nil
				}

				if skip != 0 {
					skip = skip - 1
					continue
				}

				if vol != -1 {
					if len(objects) == vol {
						break
					}
				}

				vType := fmt.Sprintf("%T", vs.([]interface{})[m])

				_, ok := d[k.(string)]
				if ok {

					if d[k.(string)] == nil {
						objects = append(objects, uint64(i))
						continue
					}

					if reflect.TypeOf(d[k.(string)]).Kind() == reflect.Slice {
						for _, dd := range d[k.(string)].([]interface{}) {

							if len(objects) == vol {
								break
							}

							if reflect.TypeOf(dd).Kind() == reflect.Float64 {
								if vType == "int" {
									var interfaceI int = int(dd.(float64))

									if oprs.([]interface{})[m] == "==" {
										if reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == "!=" {
										if !reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == ">" {
										if vType == "int" {
											if interfaceI > vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, uint64(i))

												exists:
												})()
											}
										}
									} else if oprs.([]interface{})[m] == "<" {
										if vType == "int" {
											if interfaceI < vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, uint64(i))

												exists:
												})()
											}
										}
									} else if oprs.([]interface{})[m] == ">=" {
										if vType == "int" {
											if interfaceI >= vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, uint64(i))

												exists:
												})()
											}
										}
									} else if oprs.([]interface{})[m] == "<=" {
										if vType == "int" {
											if interfaceI <= vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, uint64(i))

												exists:
												})()
											}
										}
									}
								} else if vType == "float64" {
									var interfaceI float64 = dd.(float64)

									if oprs.([]interface{})[m] == "==" {

										if bytes.Equal([]byte(fmt.Sprintf("%f", float64(interfaceI))), []byte(fmt.Sprintf("%f", float64(vs.([]interface{})[m].(float64))))) {

											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == "!=" {
										if float64(interfaceI) != vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == ">" {
										if float64(interfaceI) > vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}

									} else if oprs.([]interface{})[m] == "<" {
										if float64(interfaceI) < vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}

									} else if oprs.([]interface{})[m] == ">=" {

										if float64(interfaceI) >= vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}

									} else if oprs.([]interface{})[m] == "<=" {
										if float64(interfaceI) <= vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, uint64(i))

											exists:
											})()
										}

									}
								}
							} else if reflect.TypeOf(dd).Kind() == reflect.Map {
								//for kkk, ddd := range dd.(map[string]interface{}) {
								//	// unimplemented
								//}
							} else {
								if oprs.([]interface{})[m] == "==" {
									if reflect.DeepEqual(dd, vs.([]interface{})[m]) {
										conditionsMet += 1
										(func() {
											for _, o := range objects {
												if reflect.DeepEqual(o, d) {
													goto exists
												}
											}
											objects = append(objects, uint64(i))

										exists:
										})()
									}
								} else if oprs.([]interface{})[m] == "!=" {
									if !reflect.DeepEqual(dd, vs.([]interface{})[m]) {
										conditionsMet += 1
										(func() {
											for _, o := range objects {
												if reflect.DeepEqual(o, d) {
													goto exists
												}
											}
											objects = append(objects, uint64(i))

										exists:
										})()
									}
								}
							}

						}
					} else if vType == "int" {
						var interfaceI int = int(d[k.(string)].(float64))

						if oprs.([]interface{})[m] == "==" {
							if reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == "!=" {
							if !reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == ">" {
							if vType == "int" {
								if interfaceI > vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, uint64(i))

									exists:
									})()
								}
							}
						} else if oprs.([]interface{})[m] == "<" {
							if vType == "int" {
								if interfaceI < vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, uint64(i))

									exists:
									})()
								}
							}
						} else if oprs.([]interface{})[m] == ">=" {
							if vType == "int" {
								if interfaceI >= vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, uint64(i))

									exists:
									})()
								}
							}
						} else if oprs.([]interface{})[m] == "<=" {
							if vType == "int" {
								if interfaceI <= vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, uint64(i))

									exists:
									})()
								}
							}
						}
					} else if vType == "float64" {
						var interfaceI float64 = d[k.(string)].(float64)

						if oprs.([]interface{})[m] == "==" {

							if bytes.Equal([]byte(fmt.Sprintf("%f", float64(interfaceI))), []byte(fmt.Sprintf("%f", float64(vs.([]interface{})[m].(float64))))) {

								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == "!=" {
							if float64(interfaceI) != vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == ">" {
							if float64(interfaceI) > vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}

						} else if oprs.([]interface{})[m] == "<" {
							if float64(interfaceI) < vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}

						} else if oprs.([]interface{})[m] == ">=" {

							if float64(interfaceI) >= vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}

						} else if oprs.([]interface{})[m] == "<=" {
							if float64(interfaceI) <= vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))

								exists:
								})()
							}

						}
					} else {
						if oprs.([]interface{})[m] == "==" {
							if reflect.DeepEqual(d[k.(string)], vs.([]interface{})[m]) {
								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))
								exists:
								})()

							}
						} else if oprs.([]interface{})[m] == "!=" {
							if !reflect.DeepEqual(d[k.(string)], vs.([]interface{})[m]) {
								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, uint64(i))
								exists:
								})()
							}
						}

					}
				}
			}
		}

	}

	var deleted []interface{}

	if slices.Contains(conditions, "&&") {
		var nullObjects []interface{}

		if uint64(len(conditions)) != conditionsMet {

			if !slices.Contains(conditions, "||") {
				return nullObjects
			} else if conditionsMet > 0 {
				for _, i := range objects {
					if i < uint64(len(curode.Data.Map[collection])) {
						deleted = append(deleted, curode.Data.Map[collection][i])
						curode.Data.Writers[collection].Lock()
						curode.Data.Map[collection][i] = curode.Data.Map[collection][len(curode.Data.Map[collection])-1]
						curode.Data.Map[collection][len(curode.Data.Map[collection])-1] = nil
						curode.Data.Map[collection] = curode.Data.Map[collection][:len(curode.Data.Map[collection])-1]
						curode.Data.Writers[collection].Unlock()
					}
				}
			}
		}

	} else if conditionsMet > 0 {
		for _, i := range objects {
			if i < uint64(len(curode.Data.Map[collection])) {
				deleted = append(deleted, curode.Data.Map[collection][i])
				curode.Data.Writers[collection].Lock()
				curode.Data.Map[collection][i] = curode.Data.Map[collection][len(curode.Data.Map[collection])-1]
				curode.Data.Map[collection][len(curode.Data.Map[collection])-1] = nil
				curode.Data.Map[collection] = curode.Data.Map[collection][:len(curode.Data.Map[collection])-1]
				curode.Data.Writers[collection].Unlock()
			}
		}
	}

	return deleted
}

// Select selects documents based on provided keys, values and operations such as select * from COLL where KEY == VALUE && KEY > VALUE
func (curode *Curode) Select(collection string, ks interface{}, vs interface{}, vol int, skip int, oprs interface{}, lock bool, conditions []interface{}) []interface{} {
	if lock {
		l, ok := curode.Data.Writers[collection]
		if ok {
			l.Lock()
		}

	}

	defer func() {
		if lock {
			l, ok := curode.Data.Writers[collection]
			if ok {
				l.Unlock()
			}
		}
	}()

	var objects []interface{}

	var conditionsMet uint64
	//The && operator displays a document if all the conditions are TRUE.
	//The || operator displays a record if any of the conditions are TRUE.

	for i, d := range curode.Data.Map[collection] {
		if ks == nil && vs == nil && oprs == nil {
			if skip != 0 {
				skip = skip - 1
				continue
			}

			if vol != -1 {
				if i-1 == vol-1 {
					return objects
				}
			}

			objects = append(objects, d)
			continue
		} else {

			for m, k := range ks.([]interface{}) {

				if oprs.([]interface{})[m] == "" {
					fmt.Sprintf("Query operator required.")
					return nil
				}

				if skip != 0 {
					skip = skip - 1
					continue
				}

				if vol != -1 {
					if len(objects) == vol {
						return objects
					}
				}

				vType := fmt.Sprintf("%T", vs.([]interface{})[m])

				_, ok := d[k.(string)]
				if ok {

					if d[k.(string)] == nil {
						objects = append(objects, d)
						continue
					}

					if reflect.TypeOf(d[k.(string)]).Kind() == reflect.Slice {
						for _, dd := range d[k.(string)].([]interface{}) {

							if len(objects) == vol {
								return objects
							}

							if reflect.TypeOf(dd).Kind() == reflect.Float64 {
								if vType == "int" {
									var interfaceI int = int(dd.(float64))

									if oprs.([]interface{})[m] == "==" {
										if reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == "!=" {
										if !reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == ">" {
										if vType == "int" {
											if interfaceI > vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, d)
												exists:
												})()
											}
										}
									} else if oprs.([]interface{})[m] == "<" {
										if vType == "int" {
											if interfaceI < vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, d)
												exists:
												})()
											}
										}
									} else if oprs.([]interface{})[m] == ">=" {
										if vType == "int" {
											if interfaceI >= vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, d)
												exists:
												})()
											}
										}
									} else if oprs.([]interface{})[m] == "<=" {
										if vType == "int" {
											if interfaceI <= vs.([]interface{})[m].(int) {
												conditionsMet += 1
												(func() {
													for _, o := range objects {
														if reflect.DeepEqual(o, d) {
															goto exists
														}
													}
													objects = append(objects, d)
												exists:
												})()
											}
										}
									}
								} else if vType == "float64" {
									var interfaceI float64 = dd.(float64)

									if oprs.([]interface{})[m] == "==" {

										if bytes.Equal([]byte(fmt.Sprintf("%f", float64(interfaceI))), []byte(fmt.Sprintf("%f", float64(vs.([]interface{})[m].(float64))))) {

											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == "!=" {
										if float64(interfaceI) != vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}
									} else if oprs.([]interface{})[m] == ">" {
										if float64(interfaceI) > vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}

									} else if oprs.([]interface{})[m] == "<" {
										if float64(interfaceI) < vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}

									} else if oprs.([]interface{})[m] == ">=" {

										if float64(interfaceI) >= vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}

									} else if oprs.([]interface{})[m] == "<=" {
										if float64(interfaceI) <= vs.([]interface{})[m].(float64) {
											conditionsMet += 1
											(func() {
												for _, o := range objects {
													if reflect.DeepEqual(o, d) {
														goto exists
													}
												}
												objects = append(objects, d)
											exists:
											})()
										}

									}
								}
							} else if reflect.TypeOf(dd).Kind() == reflect.Map {
								//for kkk, ddd := range dd.(map[string]interface{}) {
								//	// unimplemented
								//}
							} else {
								if oprs.([]interface{})[m] == "==" {
									if reflect.DeepEqual(dd, vs.([]interface{})[m]) {
										conditionsMet += 1
										(func() {
											for _, o := range objects {
												if reflect.DeepEqual(o, d) {
													goto exists
												}
											}
											objects = append(objects, d)
										exists:
										})()
									}
								} else if oprs.([]interface{})[m] == "!=" {
									if !reflect.DeepEqual(dd, vs.([]interface{})[m]) {
										conditionsMet += 1
										(func() {
											for _, o := range objects {
												if reflect.DeepEqual(o, d) {
													goto exists
												}
											}
											objects = append(objects, d)
										exists:
										})()
									}
								}
							}

						}
					} else if vType == "int" {
						var interfaceI int = int(d[k.(string)].(float64))

						if oprs.([]interface{})[m] == "==" {
							if reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == "!=" {
							if !reflect.DeepEqual(interfaceI, vs.([]interface{})[m]) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == ">" {
							if vType == "int" {
								if interfaceI > vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, d)
									exists:
									})()
								}
							}
						} else if oprs.([]interface{})[m] == "<" {
							if vType == "int" {
								if interfaceI < vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, d)
									exists:
									})()
								}
							}
						} else if oprs.([]interface{})[m] == ">=" {
							if vType == "int" {
								if interfaceI >= vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, d)
									exists:
									})()
								}
							}
						} else if oprs.([]interface{})[m] == "<=" {
							if vType == "int" {
								if interfaceI <= vs.([]interface{})[m].(int) {
									conditionsMet += 1
									(func() {
										for _, o := range objects {
											if reflect.DeepEqual(o, d) {
												goto exists
											}
										}
										objects = append(objects, d)
									exists:
									})()
								}
							}
						}
					} else if vType == "float64" {
						var interfaceI float64 = d[k.(string)].(float64)

						if oprs.([]interface{})[m] == "==" {

							if bytes.Equal([]byte(fmt.Sprintf("%f", float64(interfaceI))), []byte(fmt.Sprintf("%f", float64(vs.([]interface{})[m].(float64))))) {

								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == "!=" {
							if float64(interfaceI) != vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}
						} else if oprs.([]interface{})[m] == ">" {
							if float64(interfaceI) > vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}

						} else if oprs.([]interface{})[m] == "<" {
							if float64(interfaceI) < vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}

						} else if oprs.([]interface{})[m] == ">=" {

							if float64(interfaceI) >= vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}

						} else if oprs.([]interface{})[m] == "<=" {
							if float64(interfaceI) <= vs.([]interface{})[m].(float64) {
								conditionsMet += 1
								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}

						}
					} else {
						if oprs.([]interface{})[m] == "==" {
							if reflect.DeepEqual(d[k.(string)], vs.([]interface{})[m]) {
								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()

							}
						} else if oprs.([]interface{})[m] == "!=" {
							if !reflect.DeepEqual(d[k.(string)], vs.([]interface{})[m]) {
								conditionsMet += 1

								(func() {
									for _, o := range objects {
										if reflect.DeepEqual(o, d) {
											goto exists
										}
									}
									objects = append(objects, d)
								exists:
								})()
							}
						}

					}
				}
			}
		}

	}

	if slices.Contains(conditions, "&&") {
		if uint64(len(conditions)) != conditionsMet {
			var nullObjects []interface{}

			if !slices.Contains(conditions, "||") {
				objects = nullObjects
			}
		}
	}

	return objects
}

// Insert into node collection
func (curode *Curode) Insert(collection string, jsonMap map[string]interface{}, connection *Connection) error {
	if curode.CurrentMemoryUsage() >= curode.Config.MaxMemory {
		return errors.New(fmt.Sprintf("%d node is at peak allocation", 100))
	}

	jsonStr, err := json.Marshal(jsonMap)
	if err != nil {
		return err
	}

	if strings.Contains(string(jsonStr), "[{\"") {
		return errors.New("nested JSON objects not permitted")
	} else if strings.Contains(string(jsonStr), ": {\"") {
		return errors.New("nested JSON objects not permitted")
	} else if strings.Contains(string(jsonStr), ":{\"") {
		return errors.New("nested JSON objects not permitted")
	}

	doc := make(map[string]interface{})
	err = json.Unmarshal([]byte(jsonStr), &doc)
	if err != nil {
		return err
	}
	writeMu, ok := curode.Data.Writers[collection]
	if ok {
		writeMu.Lock()
		defer writeMu.Unlock()

		curode.Data.Map[collection] = append(curode.Data.Map[collection], doc)
	} else {
		curode.Data.Writers[collection] = &sync.RWMutex{}
		curode.Data.Map[collection] = append(curode.Data.Map[collection], doc)
	}

	response := make(map[string]interface{})
	response["statusCode"] = 2000
	response["message"] = "Document inserted"

	response["insert"] = doc

	responseMap, err := json.Marshal(response)
	if err != nil {
		return err
	}

	connection.Text.PrintfLine(string(responseMap))

	return nil
}

// ConnectionEventWorker distributes connections to goroutines listening for connection events.
func (curode *Curode) ConnectionEventWorker() {
	defer curode.Wg.Done()
	for {
		if curode.Context.Err() != nil {
			return
		}

		if len(curode.ConnectionQueue) > 0 {
			for _, c := range curode.ConnectionQueue {
				curode.ConnectionChannel <- c
				recover()
				time.Sleep(time.Nanosecond * 1000000)
			}
		}
		time.Sleep(time.Nanosecond * 1000000)

	}
}

// ConnectionEventLoop listens to currently connected client events
func (curode *Curode) ConnectionEventLoop(i int) {
	defer curode.Wg.Done()
	for {
		select {
		case c := <-curode.ConnectionChannel:
			if curode.Context.Err() != nil {
				return
			}

			if c != nil {
				err := c.Conn.SetReadDeadline(time.Now().Add(time.Nanosecond * 1000000))
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					} else {
						fmt.Println(err.Error())
						return
					}
				}

				scanner := bufio.NewScanner(c.Conn) // Start a new scanner

				// Read until ; or a single 'quit'
				for scanner.Scan() {
					err := scanner.Err()
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					} else if err == io.EOF {
						curode.ConnectionQueueMu.Lock()
						delete(curode.ConnectionQueue, c.Conn.RemoteAddr().String())
						curode.ConnectionQueueMu.Unlock()
						continue
					} else {

						query := scanner.Text()

						result := make(map[string]interface{})

						err := json.Unmarshal([]byte(query), &result)
						if err != nil {
							result["statusCode"] = 4000
							result["message"] = "Unmarshalable JSON"
							r, _ := json.Marshal(result)
							c.Text.PrintfLine(string(r))
							continue
						}

						result["skip"] = 0

						action, ok := result["action"] // An action is insert, select, delete, ect..
						if ok {
							switch {
							case strings.EqualFold(action.(string), "delete"):

								if result["limit"].(string) == "*" {
									result["limit"] = -1
								} else if strings.Contains(result["limit"].(string), ",") {
									if len(strings.Split(result["limit"].(string), ",")) == 2 {
										result["skip"], err = strconv.Atoi(strings.Split(result["limit"].(string), ",")[0])
										if err != nil {
											c.Text.PrintfLine("Limit skip must be an integer. %s", err.Error())
											query = ""
											continue
										}

										if !strings.EqualFold(strings.Split(result["limit"].(string), ",")[1], "*") {
											result["limit"], err = strconv.Atoi(strings.Split(result["limit"].(string), ",")[1])
											if err != nil {
												c.Text.PrintfLine("Something went wrong. %s", err.Error())
												query = ""
												continue
											}
										} else {
											result["limit"] = -1
										}
									} else {
										c.Text.PrintfLine("Invalid limiting value.")
										query = ""
										continue
									}
								} else {
									result["limit"], err = strconv.Atoi(result["limit"].(string))
									if err != nil {
										c.Text.PrintfLine("Something went wrong. %s", err.Error())
										query = ""
										continue
									}
								}

								results := curode.Delete(result["collection"].(string), result["keys"], result["values"], result["limit"].(int), result["skip"].(int), result["oprs"], result["lock"].(bool), result["conditions"].([]interface{}))
								r, _ := json.Marshal(results)
								result["statusCode"] = 2000

								if reflect.DeepEqual(results, nil) || len(results) == 0 {
									result["message"] = "No documents deleted."
								} else {
									result["message"] = fmt.Sprintf("%d Document(s) deleted successfully.", len(results))
								}

								delete(result, "document")
								delete(result, "collection")
								delete(result, "action")
								delete(result, "key")
								delete(result, "limit")
								delete(result, "opr")
								delete(result, "value")
								delete(result, "lock")
								delete(result, "new-values")
								delete(result, "update-keys")
								delete(result, "conditions")
								delete(result, "keys")
								delete(result, "oprs")
								delete(result, "values")
								delete(result, "skip")

								result["deleted"] = results

								r, _ = json.Marshal(result)
								c.Text.PrintfLine(string(r))
								continue
							case strings.EqualFold(action.(string), "select"):

								if result["limit"].(string) == "*" {
									result["limit"] = -1
								} else if strings.Contains(result["limit"].(string), ",") {
									if len(strings.Split(result["limit"].(string), ",")) == 2 {
										result["skip"], err = strconv.Atoi(strings.Split(result["limit"].(string), ",")[0])
										if err != nil {
											c.Text.PrintfLine("Limit skip must be an integer. %s", err.Error())
											query = ""
											continue
										}

										if !strings.EqualFold(strings.Split(result["limit"].(string), ",")[1], "*") {
											result["limit"], err = strconv.Atoi(strings.Split(result["limit"].(string), ",")[1])
											if err != nil {
												c.Text.PrintfLine("Something went wrong. %s", err.Error())
												query = ""
												continue
											}
										} else {
											result["limit"] = -1
										}
									} else {
										c.Text.PrintfLine("Invalid limiting value.")
										query = ""
										continue
									}
								} else {
									result["limit"], err = strconv.Atoi(result["limit"].(string))
									if err != nil {
										c.Text.PrintfLine("Something went wrong. %s", err.Error())
										query = ""
										continue
									}
								}

								results := curode.Select(result["collection"].(string), result["keys"], result["values"], result["limit"].(int), result["skip"].(int), result["oprs"], result["lock"].(bool), result["conditions"].([]interface{}))
								r, _ := json.Marshal(results)
								c.Text.PrintfLine(string(r))
								continue
							case strings.EqualFold(action.(string), "update"):

								if result["limit"].(string) == "*" {
									result["limit"] = -1
								} else if strings.Contains(result["limit"].(string), ",") {
									if len(strings.Split(result["limit"].(string), ",")) == 2 {
										result["skip"], err = strconv.Atoi(strings.Split(result["limit"].(string), ",")[0])
										if err != nil {
											c.Text.PrintfLine("Limit skip must be an integer. %s", err.Error())
											query = ""
											continue
										}

										if !strings.EqualFold(strings.Split(result["limit"].(string), ",")[1], "*") {
											result["limit"], err = strconv.Atoi(strings.Split(result["limit"].(string), ",")[1])
											if err != nil {
												c.Text.PrintfLine("Something went wrong. %s", err.Error())
												query = ""
												continue
											}
										} else {
											result["limit"] = -1
										}
									} else {
										c.Text.PrintfLine("Invalid limiting value.")
										query = ""
										continue
									}
								} else {
									result["limit"], err = strconv.Atoi(result["limit"].(string))
									if err != nil {
										c.Text.PrintfLine("Something went wrong. %s", err.Error())
										query = ""
										continue
									}
								}

								results := curode.Update(result["collection"].(string), result["keys"].([]interface{}), result["values"].([]interface{}), result["update-keys"].([]interface{}), result["new-values"].([]interface{}), result["limit"].(int), result["skip"].(int), result["oprs"].([]interface{}), result["conditions"].([]interface{}))
								r, _ := json.Marshal(results)

								delete(result, "document")
								delete(result, "collection")
								delete(result, "action")
								delete(result, "key")
								delete(result, "limit")
								delete(result, "opr")
								delete(result, "value")
								delete(result, "lock")
								delete(result, "new-values")
								delete(result, "update-keys")
								delete(result, "conditions")
								delete(result, "keys")
								delete(result, "oprs")
								delete(result, "values")
								delete(result, "skip")

								result["statusCode"] = 2000

								if reflect.DeepEqual(results, nil) || len(results) == 0 {
									result["message"] = "No documents updated."
								} else {
									result["message"] = fmt.Sprintf("%d Document(s) updated successfully.", len(results))
								}

								result["updated"] = results
								r, _ = json.Marshal(result)

								c.Text.PrintfLine(string(r))
								continue
							case strings.EqualFold(action.(string), "insert"):

								collection := result["collection"]
								doc := result["document"]
								delete(result, "document")
								delete(result, "collection")
								delete(result, "action")
								delete(result, "skip")

								err := curode.Insert(collection.(string), doc.(map[string]interface{}), c)
								if err != nil {
									// Only error returned is a 4003 which means cannot insert nested object
									result["statusCode"] = 4003
									result["message"] = err.Error()
									r, _ := json.Marshal(result)
									c.Text.PrintfLine(string(r))
									continue
								}

								continue
							default:

								result["statusCode"] = 4002
								result["message"] = "Invalid/Non-existent action"
								r, _ := json.Marshal(result)

								c.Text.PrintfLine(string(r))
								continue
							}
						} else {
							result["statusCode"] = 4001
							result["message"] = "Missing action" // Missing select, insert
							r, _ := json.Marshal(result)

							c.Text.PrintfLine(string(r))
							continue
						}
					}
				}

			}
		}
	}
}

// SignalListener listeners for system signals are does a graceful shutdown
func (curode *Curode) SignalListener() {
	defer curode.Wg.Done()
	for {
		select {
		case sig := <-curode.SignalChannel:
			log.Println("received", sig)
			curode.ContextCancel()

			return

		default:
			time.Sleep(time.Millisecond * 1)
		}
	}
}

// main is the starting point for the CursusDB node software
func main() {
	var curode Curode // Node type variable

	curode.Data.Map = make(map[string][]map[string]interface{}) // Main hashmap
	curode.Data.Writers = make(map[string]*sync.RWMutex)        // Read/Write mutexes per collection

	curode.ConnectionQueue = make(map[string]*Connection) // Make connection queue [remote_addr_str]*Connection
	curode.ConnectionQueueMu = &sync.RWMutex{}            // Cluster connection queue mutex
	curode.ConnectionChannel = make(chan *Connection)
	curode.Context, curode.ContextCancel = context.WithCancel(context.Background())

	// Check if .curodeconfig exists
	if _, err := os.Stat("./.curodeconfig"); errors.Is(err, os.ErrNotExist) {

		// Create .curodeconfig
		nodeConfigFile, err := os.OpenFile("./.curodeconfig", os.O_CREATE|os.O_RDWR, 0777)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		// Defer close node config
		defer nodeConfigFile.Close()

		curode.Config.Port = 7682       // Set default CursusDB node port
		curode.Config.MaxMemory = 10240 // Max memory 10GB default
		curode.Config.ConnectionQueueWorkers = 4
		curode.Config.Host = "0.0.0.0"

		fmt.Println("Node key is required.  A node key is shared with your cluster and will encrypt all your data at rest and allow for only connections that contain a correct Key: header value matching the hashed key you provide.")
		fmt.Print("key> ")
		key, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		// Repear key with * so Alex would be ****
		fmt.Print(strings.Repeat("*", utf8.RuneCountInString(string(key))))
		fmt.Println("")

		// Hash and encode key
		hashedKey := sha256.Sum256(key)
		curode.Config.Key = base64.StdEncoding.EncodeToString(append([]byte{}, hashedKey[:]...))

		// Marshal node config into yaml
		yamlData, err := yaml.Marshal(&curode.Config)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		// Write to node config
		nodeConfigFile.Write(yamlData)
	} else {
		// Read node config
		nodeConfigFile, err := os.ReadFile("./.curodeconfig")
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		// Unmarshal node config yaml
		err = yaml.Unmarshal(nodeConfigFile, &curode.Config)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

	}

	// Read rested data from .cdat file
	if _, err := os.Stat("./.cdat"); errors.Is(err, os.ErrNotExist) { // Not exists we create it
		fmt.Println("No previous data to read.  Creating new .cdat file.")
	} else {
		fmt.Println("Node data read into memory.")
		dataFile, err := os.Open("./.cdat") // Open .cdat

		// Temporary decrypted data file.. to be unserialized into map
		fDFTmp, err := os.OpenFile(".cdat.tmp", os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0777)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		// Read encrypted data file
		reader := bufio.NewReader(dataFile)
		buf := make([]byte, 1024)

		defer dataFile.Close()

		for {
			read, err := reader.Read(buf)

			if err != nil {
				if err != io.EOF {
					fmt.Println(err.Error())
					os.Exit(1)
				}
				break
			}

			if read > 0 {
				decodedKey, err := base64.StdEncoding.DecodeString(curode.Config.Key)
				if err != nil {
					fmt.Println(err.Error())
					os.Exit(1)
					return
				}

				serialized, err := curode.Decrypt(decodedKey[:], buf[:read])
				if err != nil {
					fmt.Println(err.Error())
					os.Exit(1)
					return
				}

				fDFTmp.Write(serialized) // Decrypt serialized
			}
		}
		fDFTmp.Close()

		fDFTmp, err = os.OpenFile(".cdat.tmp", os.O_RDONLY, 0777)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		d := gob.NewDecoder(fDFTmp)

		// Now with all serialized data we encode into data hashmap
		err = d.Decode(&curode.Data.Map)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		fDFTmp.Close()

		os.Remove(".cdat.tmp") // Remove temp
	}

	// Parse flags
	flag.IntVar(&curode.Config.Port, "port", curode.Config.Port, "port for node")
	flag.Parse()

	curode.SignalChannel = make(chan os.Signal, 1) // Create signal channel

	signal.Notify(curode.SignalChannel, syscall.SIGINT, syscall.SIGTERM)
	curode.Wg = &sync.WaitGroup{} // Create wait group

	curode.Wg.Add(1)
	go curode.SignalListener()

	curode.Wg.Add(1)
	go curode.StartTCP_TLSListener() // Listen to tcp or tls cluster connections

	curode.Wg.Add(1)
	go curode.ConnectionEventWorker()

	// Start up connection queue goroutines in which listen for events.
	for i := curode.Config.ConnectionQueueWorkers; i > 0; i-- { // Get amount of workers based on config
		curode.Wg.Add(1)
		go curode.ConnectionEventLoop(i + 1)
	}

	curode.Wg.Wait() // Wait for all go routines

}
