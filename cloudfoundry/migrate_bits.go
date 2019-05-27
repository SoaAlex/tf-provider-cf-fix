package cloudfoundry

import (
	"crypto/tls"
	"fmt"
	"github.com/ArthurHlt/zipper"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/terraform"
	"github.com/terraform-providers/terraform-provider-cloudfoundry/cloudfoundry/managers"
	"github.com/whilp/git-urls"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var folderBits = "bits"

func schemaOldBits() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"url": &schema.Schema{
			Type:     schema.TypeString,
			Optional: true,
		},
		"git": &schema.Schema{
			Type:     schema.TypeList,
			Optional: true,
			MaxItems: 1,
			Elem: &schema.Resource{
				Schema: map[string]*schema.Schema{
					"url": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
					"branch": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
						Default:  "master",
					},
					"tag": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"user": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"password": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"key": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
				},
			},
		},
		"github_release": &schema.Schema{
			Type:     schema.TypeList,
			Optional: true,
			MaxItems: 1,
			Elem: &schema.Resource{
				Schema: map[string]*schema.Schema{
					"owner": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
					"repo": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
					"user": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"password": &schema.Schema{
						Type:     schema.TypeString,
						Optional: true,
					},
					"version": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
					"filename": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
				},
			},
		},
		"add_content": &schema.Schema{
			Type:     schema.TypeList,
			Optional: true,
			Elem: &schema.Resource{
				Schema: map[string]*schema.Schema{
					"source": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
					"destination": &schema.Schema{
						Type:     schema.TypeString,
						Required: true,
					},
				},
			},
		},
	}
}

func zipperManager(meta interface{}) (*zipper.Manager, error) {
	m, err := zipper.NewManager(zipper.NewGitHandler(), &zipper.HttpHandler{}, &zipper.LocalHandler{})
	if err != nil {
		return nil, err
	}
	m.SetHttpClient(&http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: meta.(*managers.Session).Config.SkipSslValidation,
			},
		},
	})
	return m, nil
}

func migrateBitsStateV2toV3(is *terraform.InstanceState, meta interface{}) (*terraform.InstanceState, error) {
	if is.Empty() {
		log.Println("[DEBUG] Empty InstanceState; nothing to migrate.")
		return is, nil
	}
	reader := &schema.MapFieldReader{
		Schema: schemaOldBits(),
		Map:    schema.BasicMapReader(is.Attributes),
	}

	result, err := reader.ReadField([]string{"url"})
	if err != nil {
		return is, err
	}
	rawUrl := ""
	if result.Exists {
		rawUrl = result.Value.(string)
	}
	result, err = reader.ReadField([]string{"git"})
	if err != nil {
		return is, err
	}
	git := make(map[string]interface{})
	gitElems := getListOfStructs(result.Value)
	if len(gitElems) > 0 {
		git = gitElems[0]
	}

	result, err = reader.ReadField([]string{"github_release"})
	if err != nil {
		return is, err
	}
	github := make(map[string]interface{})
	githubElems := getListOfStructs(result.Value)
	if len(githubElems) > 0 {
		github = githubElems[0]
	}

	result, err = reader.ReadField([]string{"add_content"})
	if err != nil {
		return is, err
	}
	addContents := getListOfStructs(result.Value)
	if len(addContents) > 0 {
		return is, fmt.Errorf("add_content attribute can't be migrate, please fixeit in other way and remove from terraform.tfstate add_content attributes")
	}

	u, err := migrateBitsUrl(rawUrl, git, github)
	if err != nil {
		return is, err
	}
	if u == nil {
		return is, nil
	}

	if (u.Scheme == "http" || u.Scheme == "https") && filepath.Ext(u.Path) == ".zip" {
		is.Attributes["path"] = u.String()
		is.Attributes = migrateBitsDeleteAttr(is.Attributes)
		return is, nil
	}

	zipMan, err := zipperManager(meta)
	if err != nil {
		return is, err
	}
	handlerName := ""
	if u.Host == "" {
		handlerName = "local"
	}
	s, err := zipMan.CreateSession(u.String(), handlerName)
	if err != nil {
		return is, err
	}

	sha1, err := s.Sha1()
	if err != nil {
		return is, err
	}
	is.Attributes["source_code_hash"] = sha1

	outputPath := filepath.Join(folderBits, migrateBitsOutputPath(u))
	err = os.MkdirAll(filepath.Dir(outputPath), os.ModePerm)
	if err != nil {
		return is, err
	}
	log.Printf("[DEBUG] output path %s\n", outputPath)
	zipFile, err := s.Zip()
	if err != nil {
		return is, err
	}
	defer zipFile.Close()

	f, err := os.Create(outputPath)
	if err != nil {
		return is, err
	}
	defer f.Close()

	_, err = io.Copy(f, zipFile)
	if err != nil {
		return is, err
	}

	is.Attributes["path"] = outputPath
	is.Attributes = migrateBitsDeleteAttr(is.Attributes)
	log.Printf("[DEBUG] Attributes after migration: %#v", is.Attributes)
	return is, nil
}

func migrateBitsOutputPath(u *url.URL) string {
	path := u.Path
	if filepath.Base(path) == "zip" || filepath.Base(path) == "tar" {
		path = filepath.Join(filepath.Dir(path), "archive.zip")
	}
	if path != "" {
		path = strings.TrimSuffix(path, filepath.Ext(path)) + ".zip"
	}
	host := u.Host
	if host == "glare.now.sh" {
		host = "github.com"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if host == "" {
		return filepath.Base(path)
	}
	if path == "" {
		return filepath.FromSlash(host) + ".zip"
	}

	return filepath.FromSlash(strings.TrimSuffix(host, "/") + path)
}

func migrateBitsUrl(rawUrl string, git, github map[string]interface{}) (*url.URL, error) {
	if rawUrl != "" {
		rawUrl = strings.TrimPrefix(rawUrl, "file://")
		u, err := url.Parse(rawUrl)
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	if len(git) > 0 {
		u, err := giturls.Parse(git["url"].(string))
		if err != nil {
			return nil, err
		}
		if git["branch"].(string) != "" || git["tag"].(string) != "" {
			u.Fragment = git["branch"].(string) + git["tag"].(string)
		}
		if git["user"].(string) != "" {
			u.User = url.UserPassword(git["user"].(string), git["password"].(string))
		}
		return u, nil
	}
	if len(github) == 0 {
		return nil, nil
	}
	version := github["version"].(string)
	filename := github["filename"].(string)
	owner := github["owner"].(string)
	repo := github["repo"].(string)
	var u *url.URL
	var err error
	if version != "" && filename == "zipball" {
		u, err = url.Parse(fmt.Sprintf("https://github.com/%s/%s/archive/%s.zip", owner, repo, version))
	} else if version != "" && filename == "tarball" {
		u, err = url.Parse(fmt.Sprintf("https://github.com/%s/%s/archive/%s.tar.gz", owner, repo, version))
	} else if version != "" {
		u, err = url.Parse(fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", owner, repo, version, filename))
	} else if filename == "zipball" {
		u, err = url.Parse(fmt.Sprintf("https://glare.now.sh/%s/%s/zip", owner, repo))
	} else if filename == "tarball" {
		u, err = url.Parse(fmt.Sprintf("https://glare.now.sh/%s/%s/tar", owner, repo))
	} else {
		u, err = url.Parse(fmt.Sprintf("https://glare.now.sh/%s/%s/%s", owner, repo, filename))
	}
	if err != nil {
		return nil, err
	}
	log.Printf("[DEBUG] url %s\n", u.String())
	if github["user"].(string) != "" {
		u.User = url.UserPassword(github["user"].(string), github["password"].(string))
	}
	return u, nil
}
func migrateBitsDeleteAttr(m map[string]string) map[string]string {
	delete(m, "add_content")
	delete(m, "url")
	m = cleanByKeyAttribute("git", m)
	m = cleanByKeyAttribute("github_release", m)
	return m
}
