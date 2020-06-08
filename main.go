package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	rice "github.com/GeertJohan/go.rice"
	"github.com/gorilla/websocket"
	uuid "github.com/satori/go.uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	addr       = flag.String("addr", "127.0.0.1:8004", "http service address")
	kubeconfig = flag.String("kubeconfig", "", "Path to the kubeconfig file.")
	toastTypes = [...]string{"alert", "success", "error", "warning", "info"}
	cmd        = []string{"/bin/sh", "-c", "TERM=xterm-256color; export TERM; [ -x /bin/bash ] && ([ -x /usr/bin/script ] && /usr/bin/script -q -c \"/bin/bash\" /dev/null || exec /bin/bash) || exec /bin/sh"}
	upgrader   = websocket.Upgrader{}
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = 5 * time.Second

	// End of transmission (Ctrl-C Ctrl-C Ctrl-D Ctrl-D).
	endOfTransmission = "\u0003\u0003\u0004\u0004"
)

// PtyHandler is what remotecommand expects from a pty
type PtyHandler interface {
	io.Reader
	io.Writer
	remotecommand.TerminalSizeQueue
}

// TerminalSession implements PtyHandler
type TerminalSession struct {
	id        string
	ws        *websocket.Conn
	l         *sync.Mutex
	reqParams *ReqParams
	sizeChan  chan remotecommand.TerminalSize
	doneChan  chan struct{}
}

// TerminalMessage is the messaging protocol between ShellController and TerminalSession.
//
// OP      DIRECTION  FIELD(S) USED  DESCRIPTION
// ---------------------------------------------------------------------
// bind    fe->be     SessionID      Id sent back from TerminalResponse
// stdin   fe->be     Data           Keystrokes/paste buffer
// resize  fe->be     Rows, Cols     New terminal size
// stdout  be->fe     Data           Output from the process
// toast   be->fe     Data           OOB message to be shown to the user
//
// ToastType
// alert, success, error, warning, info - ClassName generator uses this value → noty_type__${type}
type TerminalMessage struct {
	Op, Data, SessionID, ToastType string
	Rows, Cols                     uint16
}

// ReqParams is query params
type ReqParams struct {
	Context     string `json:"context"`
	Namespace   string `json:"namespace"`
	PodID       string `json:"pod"`
	ContainerID string `json:"container"`
	Resource    string `json:"resource"`
	TailLines   string `json:"tailLines"`
	Timeout     int    `json:"timeout"`
}

// Next handles pty->process resize events
// Called in a loop from remotecommand as long as the process is running
func (t TerminalSession) Next() *remotecommand.TerminalSize {
	select {
	case size := <-t.sizeChan:
		return &size
	case <-t.doneChan:
		return nil
	}
}

// Read handles pty->process messages (stdin, resize)
// Called in a loop from remotecommand as long as the process is running
func (t TerminalSession) Read(p []byte) (int, error) {
	t.ws.SetReadDeadline(time.Now().Add(time.Second * time.Duration(t.reqParams.Timeout)))
	_, m, err := t.ws.ReadMessage()
	if err != nil {
		log.Println(err)
		// Send terminated signal to process to avoid resource leak
		t.Toast("error", err.Error())
		return copy(p, endOfTransmission), err
	}

	var msg TerminalMessage
	if err := json.Unmarshal([]byte(m), &msg); err != nil {
		log.Println(err)
		t.Toast("error", err.Error())
		return copy(p, endOfTransmission), err
	}

	switch msg.Op {
	case "stdin":
		return copy(p, msg.Data), nil
	case "resize":
		t.sizeChan <- remotecommand.TerminalSize{Width: msg.Cols, Height: msg.Rows}
		return 0, nil
	default:
		t.Toast("error", "unknown op")
		return copy(p, endOfTransmission), fmt.Errorf("unknown message type '%s'", msg.Op)
	}
}

// Write handles process->pty stdout
// Called from remotecommand whenever there is any output
func (t TerminalSession) Write(p []byte) (int, error) {
	// 避免 Write Toast 同时写(Observed a panic: concurrent write to websocket connection)
	t.l.Lock()
	defer t.l.Unlock()
	t.ws.SetWriteDeadline(time.Now().Add(writeWait))
	msg, err := json.Marshal(TerminalMessage{
		SessionID: t.id,
		Op:        "stdout",
		Data:      string(p),
	})
	if err != nil {
		log.Println(err)
		return 0, err
	}
	if err = t.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
		log.Println(err)
		return 0, err
	}
	return len(p), nil
}

// Toast can be used to send the user any OOB messages
// 前端使用 Noty 显示
func (t TerminalSession) Toast(toastType, p string) error {
	t.l.Lock()
	defer t.l.Unlock()
	t.ws.SetWriteDeadline(time.Now().Add(writeWait))
	index := -1
	for i, x := range toastTypes {
		if toastType == x {
			index = i
			break
		}
	}
	if index == -1 {
		toastType = toastTypes[0]
	}
	msg, err := json.Marshal(TerminalMessage{
		Op:        "toast",
		Data:      p,
		ToastType: toastType,
	})
	if err != nil {
		return err
	}

	if err = t.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
		return err
	}
	return nil
}

func parseReq(r *http.Request) *ReqParams {
	query := r.URL.Query()
	timeout, err := strconv.Atoi(query.Get("timeout"))
	if err == nil {
		if timeout <= 0 || timeout > 600 {
			timeout = 2 * 60
		}
	} else {
		timeout = 2 * 60
	}
	tailLines, err := strconv.Atoi(query.Get("tailLines"))
	if err == nil {
		if tailLines <= 0 || tailLines > 1000 {
			tailLines = 500
		}
	} else {
		tailLines = 500
	}
	p := ReqParams{
		Context:     query.Get("context"),
		Namespace:   query.Get("namespace"),
		PodID:       query.Get("pod"),
		ContainerID: query.Get("container"),
		Timeout:     timeout,
		TailLines:   strconv.Itoa(tailLines),
		Resource:    query.Get("resource"),
	}
	return &p
}

// 读取历史 log
func readLogs(t *TerminalSession, clientset *kubernetes.Clientset) error {
	reqLog := clientset.CoreV1().RESTClient().Get().
		Resource("pods").
		Name(t.reqParams.PodID).
		Namespace(t.reqParams.Namespace).
		SubResource("log").
		Param("tailLines", t.reqParams.TailLines)
	if len(t.reqParams.ContainerID) > 0 {
		reqLog.VersionedParams(&corev1.PodLogOptions{
			Container: t.reqParams.ContainerID,
		}, scheme.ParameterCodec)
	}
	readCloser, err := reqLog.Stream()
	if err != nil {
		return err
	}
	defer readCloser.Close()
	log, err := ioutil.ReadAll(readCloser)
	if err != nil {
		return err
	}
	t.Write(log)
	return nil
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	defer ws.Close()

	sessionID := uuid.NewV4().String()

	p := parseReq(r)
	t := &TerminalSession{
		id:        sessionID,
		sizeChan:  make(chan remotecommand.TerminalSize),
		ws:        ws,
		l:         &sync.Mutex{},
		reqParams: p,
	}
	defer t.Toast("warning", "connection close")
	if p.Resource != "exec" && p.Resource != "attach" {
		t.Toast("error", "resource error")
		return
	}

	// err = Auth(r, p)
	// if err != nil {
	// 	t.Toast("error", err.Error())
	// 	return
	// }

	clientset, config, err := buildClientSet(p.Context)
	if err != nil {
		t.Toast("error", err.Error())
		return
	}

	// 如果没有指定容器名的，使用第一个容器
	if len(p.PodID) != 0 && len(p.ContainerID) == 0 {
		Pod, err := clientset.CoreV1().Pods(p.Namespace).Get(p.PodID, metav1.GetOptions{})
		if err == nil {
			p.ContainerID = Pod.Spec.Containers[0].Name
		}
	}

	t.Toast("info", "connected")

	if t.reqParams.Resource == "attach" {
		err = readLogs(t, clientset)
		if err != nil {
			t.Toast("error", err.Error())
			return
		}
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(t.reqParams.PodID).
		Namespace(t.reqParams.Namespace).
		SubResource(t.reqParams.Resource)

	tty := false
	if t.reqParams.Resource == "exec" {
		tty = true
		req.VersionedParams(&corev1.PodExecOptions{
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
			Command:   cmd,
			Container: t.reqParams.ContainerID,
		}, scheme.ParameterCodec)
	} else {
		req.VersionedParams(&corev1.PodAttachOptions{
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			Container: t.reqParams.ContainerID,
		}, scheme.ParameterCodec)
	}

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Toast("error", err.Error())
		return
	}

	done := make(chan struct{})
	go ping(t, done)
	defer close(done)

	ptyHandler := PtyHandler(t)
	if t.reqParams.Resource == "exec" {
		err = exec.Stream(remotecommand.StreamOptions{
			Stdin:             ptyHandler,
			Stdout:            ptyHandler,
			Stderr:            ptyHandler,
			TerminalSizeQueue: ptyHandler,
			Tty:               tty,
		})
	} else {
		err = exec.Stream(remotecommand.StreamOptions{
			// Stdin:  ptyHandler,
			Stdout:            ptyHandler,
			Stderr:            ptyHandler,
			TerminalSizeQueue: ptyHandler,
			Tty:               tty,
		})
	}

	if err != nil {
		t.Toast("error", err.Error())
		return
	}

}

func ping(t *TerminalSession, done chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// 保持与客户端连接不断开
			if err := t.ws.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(writeWait)); err != nil {
				log.Println(err)
				return
			}
			// 保持与 kubernetes 连接不断开
			if t.reqParams.Resource == "exec" {
				t.sizeChan <- remotecommand.TerminalSize{}
			}
		case <-done:
			return
		}
	}
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func buildClientSet(context string) (*kubernetes.Clientset, *rest.Config, error) {
	if home := homeDir(); home != "" && *kubeconfig == "" {
		*kubeconfig = filepath.Join(home, ".kube", "config")
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig},
		&clientcmd.ConfigOverrides{
			CurrentContext: context,
		}).ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	return clientset, config, err
}

func main() {
	flag.Parse()

	// 内嵌静态资源
	box := rice.MustFindBox("public")
	http.Handle("/", http.FileServer(box.HTTPBox()))

	http.HandleFunc("/ws", serveWs)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
