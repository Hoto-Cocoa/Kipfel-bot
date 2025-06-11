package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

type Backlink struct {
	Document string `json:"document"`
	Flags    string `json:"flags"`
}

type BacklinkResponse struct {
	Backlinks []Backlink `json:"backlinks"`
}

type Discuss struct {
	Slug        string `json:"slug"`
	Topic       string `json:"topic"`
	UpdatedDate int    `json:"updated_date"`
	Status      string `json:"status"`
}

func main() {
	cfg, err := ini.Load("config.ini")
	if err != nil {
		cfg = ini.Empty()
		domain, token := promptConfig()
		cfg.Section("").Key("domain").SetValue(domain)
		cfg.Section("").Key("token").SetValue(token)
		cfg.SaveTo("config.ini")
	}
	domain := cfg.Section("").Key("domain").String()
	token := cfg.Section("").Key("token").String()

	dataCfg, err := ini.Load("data.ini")
	if err != nil {
		dataCfg = ini.Empty()
		nsInput := prompt("찾아볼 이름공간을 쉼표로 구분하여 입력하세요: ")
		logTpl := prompt("편집 요약 형식을 입력하세요({old}, {new} 사용): ")
		watchDoc := prompt("열린 토론을 감시할 문서의 표제어를 입력하세요: ")
		dataCfg.Section("").Key("namespaces").SetValue(nsInput)
		dataCfg.Section("").Key("logTemplate").SetValue(logTpl)
		dataCfg.Section("").Key("watchDocument").SetValue(watchDoc)
		dataCfg.SaveTo("data.ini")
	}
	nsList := parseList(dataCfg.Section("").Key("namespaces").String())
	logTemplate := dataCfg.Section("").Key("logTemplate").String()
	watchDocument := dataCfg.Section("").Key("watchDocument").String()

	go func() {
		for {
			open, err := checkDiscuss(domain, token, watchDocument)
			if err != nil {
				fmt.Fprintf(os.Stderr, "토론 확인 중 오류 발생: %v\n", err)
				panic(err)
			} else if open {
				fmt.Printf("[[%s]] 문서에 열린 토론이 있어 봇을 정지합니다.\n", watchDocument)
				os.Exit(0)
			}
			time.Sleep(15 * time.Second)
		}
	}()

	oldTitle := prompt("기존 표제어를 입력하세요: ")
	newTitle := prompt("신규 표제어를 입력하세요: ")
	keepText := strings.ToLower(prompt("표시 텍스트를 기존 표제어로 유지하시겠습니까? (y/n): ")) == "y"

	logEntry := strings.ReplaceAll(logTemplate, "{old}", oldTitle)
	logEntry = strings.ReplaceAll(logEntry, "{new}", newTitle)

	docsMap := make(map[string]struct{})
	for _, ns := range nsList {
		list, err := getBacklinksByNamespace(domain, token, oldTitle, ns)
		if err != nil {
			fmt.Printf("'%s' 이름공간에서 역링크를 탐색하는 도중 오류가 발생했습니다: %v\n", ns, err)
			continue
		}
		for _, doc := range list {
			docsMap[doc] = struct{}{}
		}
	}
	var docs []string
	for doc := range docsMap {
		docs = append(docs, doc)
	}
	total := len(docs)
	fmt.Printf("%d개의 역링크를 찾았습니다.\n", total)

	re := regexp.MustCompile(`\[\[[\t\f ]*` + regexp.QuoteMeta(oldTitle) + `[\t\f ]*(?:\|([^\[\]]+))?\]\]`)
	for idx, doc := range docs {
		text, editToken, err := getPageContent(domain, token, doc)
		if err != nil {
			if err == ErrPermDenied {
				fmt.Printf("권한 문제로 [[%s]] 문서를 편집할 수 없습니다. (%d/%d)\n", doc, idx+1, total)
			} else {
				fmt.Printf("[[%s]] 문서 편집을 실패했습니다. (%d/%d)\n%v\n", doc, idx+1, total, err)
			}
			continue
		}
		updated := re.ReplaceAllStringFunc(text, func(m string) string {
			parts := re.FindStringSubmatch(m)
			if parts[1] == newTitle {
				parts[1] = ""
			}
			if parts[1] != "" {
				return fmt.Sprintf("[[%s|%s]]", newTitle, parts[1])
			}
			if keepText {
				return fmt.Sprintf("[[%s|%s]]", newTitle, oldTitle)
			}
			return fmt.Sprintf("[[%s]]", newTitle)
		})
		if updated != text {
			err = updatePageContent(domain, token, doc, updated, editToken, logEntry)
			if err != nil {
				fmt.Printf("[[%s]] 문서 편집을 실패했습니다. (%d/%d)\n%v\n", doc, idx+1, total, err)
			} else {
				fmt.Printf("[[%s]] 문서 편집을 성공했습니다. (%d/%d)\n", doc, idx+1, total)
			}
			time.Sleep(time.Second)
		}
	}
}

func promptConfig() (string, string) {
	d := prompt("도메인을 입력하세요(예시: theseed.io): ")
	t := prompt("API 토큰을 입력하세요: ")
	return d, t
}

func prompt(msg string) string {
	fmt.Print(msg)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func parseList(s string) []string {
	parts := strings.Split(s, ",")
	var list []string
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			list = append(list, t)
		}
	}
	return list
}

func getBacklinksByNamespace(domain, token, title, namespace string) ([]string, error) {
	urlStr := fmt.Sprintf("https://%s/api/backlink/%s?namespace=%s", domain,
		url.PathEscape(title), url.QueryEscape(namespace))
	req, _ := http.NewRequest("GET", urlStr, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res BacklinkResponse
	json.Unmarshal(body, &res)
	var docs []string
	for _, b := range res.Backlinks {
		if b.Flags == "link" {
			docs = append(docs, b.Document)
		}
	}
	return docs, nil
}

func checkDiscuss(domain, token, title string) (bool, error) {
	urlStr := fmt.Sprintf("https://%s/api/discuss/%s", domain, url.PathEscape(title))
	req, _ := http.NewRequest("GET", urlStr, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var discussList []Discuss
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &discussList)

	for _, d := range discussList {
		if d.Status == "normal" {
			return true, nil
		}
	}

	return false, nil
}

var ErrPermDenied = errors.New("API access denied due to insufficient permissions")

func getPageContent(domain, token, title string) (string, string, error) {
	urlStr := fmt.Sprintf("https://%s/api/edit/%s", domain, url.PathEscape(title))
	req, _ := http.NewRequest("GET", urlStr, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Text   string `json:"text"`
		Token  string `json:"token"`
		Status string `json:"status"`
	}
	json.Unmarshal(body, &r)
	if strings.Contains(r.Status, "때문에 편집 권한이 부족합니다.") {
		return "", "", ErrPermDenied
	}
	return r.Text, r.Token, nil
}

func updatePageContent(domain, token, title, content, editToken, logMsg string) error {
	payload := map[string]string{"text": content, "log": logMsg, "token": editToken}
	data, _ := json.Marshal(payload)
	urlStr := fmt.Sprintf("https://%s/api/edit/%s", domain, url.PathEscape(title))
	req, _ := http.NewRequest("POST", urlStr, strings.NewReader(string(data)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}
