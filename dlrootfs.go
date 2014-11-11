package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"strings"

	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/registry"
)

const (
	VERSION            string = "1.3"
	MAX_DL_CONCURRENCY int    = 7
)

var (
	rootfsDest    *string = flag.String("d", "./rootfs", "destination of the resulting rootfs directory")
	imageFullName *string = flag.String("i", "", "name of the image")
	version       *bool   = flag.Bool("v", false, "display dlrootfs version")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: dlrootfs -i <image_name>:[<image_tag>] [-d <rootfs_destination>]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "\tdlrootfs -i ubuntu  #if no tag, use latest\n")
		fmt.Fprintf(os.Stderr, "\tdlrootfs -i ubuntu:precise\n")
		fmt.Fprintf(os.Stderr, "\tdlrootfs -i dockefile/elasticsearch:latest\n")
		fmt.Fprintf(os.Stderr, "Default:\n")
		flag.PrintDefaults()
	}
}

func assertErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {

	flag.Parse()

	if *version {
		fmt.Println(VERSION)
		return
	}

	if *imageFullName == "" {
		flag.Usage()
		return
	}

	var imageName string
	var imageTag string

	if strings.Contains(*imageFullName, ":") {
		imageName = strings.Split(*imageFullName, ":")[0]
		imageTag = strings.Split(*imageFullName, ":")[1]
	} else {
		imageName = *imageFullName
		imageTag = "latest"
	}

	fmt.Printf("Requesting image: %v:%v\n", imageName, imageTag)

	//resolving endpoint
	registryEndpoint, err := resolveEndpointForImage(imageName)
	assertErr(err)

	fmt.Printf("Endpoint: %v\nAPI: %v\n", registryEndpoint.URL, registryEndpoint.Version)

	session, err := openSession(registryEndpoint)
	assertErr(err)

	//Get back token and endpoint for the repository
	repoData, err := session.GetRepositoryData(imageName)
	assertErr(err)

	fmt.Printf("Download information: %v (tokens: %v)\n", repoData.Endpoints, repoData.Tokens)

	tagsList, err := session.GetRemoteTags(repoData.Endpoints, imageName, repoData.Tokens)
	assertErr(err)
	imageId := tagsList[imageTag]
	fmt.Printf("Image ID: %v\n", imageId)

	//Download image history (get back all the layers)
	history, err := session.GetRemoteHistory(imageId, repoData.Endpoints[0], repoData.Tokens)
	assertErr(err)

	err = os.MkdirAll(*rootfsDest, 0700)
	assertErr(err)

	var lastImageData []byte

	queue := NewQueue(MAX_DL_CONCURRENCY)

	fmt.Printf("Downloading %d layers:\n", len(history))

	for i := len(history) - 1; i >= 0; i-- {
		layerId := history[i]
		job := NewDownloadJob(session, repoData, layerId)
		queue.Enqueue(job)
	}
	<-queue.DoneChan

	fmt.Printf("Untaring downloaded layers:\n")

	//do not extract metadata file (i.e: .wh..wh.aufs, .wh..wh.orph, .wh..wh.plnk)
	//no lchown if not on linux
	tarOptions := &archive.TarOptions{NoLchown: false, Excludes: []string{".wh."}}
	if runtime.GOOS != "linux" {
		tarOptions.NoLchown = true
	}

	for i := len(history) - 1; i >= 0; i-- {
		layerId := history[i]
		fmt.Printf("\t%v ... ", layerId)
		job := queue.CompletedJobWithID(layerId).(*DownloadJob)
		err = archive.Untar(job.LayerData, *rootfsDest, tarOptions)
		job.LayerData.Close()
		assertErr(err)
		if i == 0 {
			lastImageData = job.LayerInfo
		}
		fmt.Printf("done\n")
	}

	var imageInfo map[string]interface{}
	err = json.Unmarshal(lastImageData, &imageInfo)
	assertErr(err)
	prettyInfo, _ := json.MarshalIndent(imageInfo, "", "  ")
	ioutil.WriteFile(*rootfsDest+"/container_info.json", prettyInfo, 0644)

	fmt.Printf("\nRootfs of %v:%v in %v\n", imageName, imageTag, *rootfsDest)
}

func openSession(endpoint *registry.Endpoint) (*registry.Session, error) {
	//opening a session
	//empty auth config (probably used only for private repository or private images I guess)
	authConfig := &registry.AuthConfig{}
	var metaHeaders map[string][]string

	return registry.NewSession(authConfig, registry.HTTPRequestFactory(metaHeaders), endpoint, true)
}

func resolveEndpointForImage(imageName string) (*registry.Endpoint, error) {
	hostname, _, err := registry.ResolveRepositoryName(imageName)
	if err != nil {
		return nil, err
	}
	return registry.NewEndpoint(hostname)
}
