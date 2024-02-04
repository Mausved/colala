package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/organizationmanager/v1"
	"github.com/yandex-cloud/go-genproto/yandex/cloud/resourcemanager/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const tgBotToken = "6749520657:AAFTRfH100OjgU3BH-MRtt5XLujnwAJGjB4"

var (
	startVmPattern = regexp.MustCompile(`start-.*`)
	stopVmPattern  = regexp.MustCompile(`stop-.*`)
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var yandex *yandexCloudApiClient

	yandex, err := newYandexCloudApiClient()
	if err != nil {
		log.Fatalf("failed new yandex cloud api client: %v", err)
	}

	mu := &sync.RWMutex{}

	go func() {
		ticker := time.NewTicker(3 * time.Hour)

		updYandexClient := func() {
			mu.Lock()
			defer mu.Unlock()
			yandex, err = newYandexCloudApiClient()
			if err != nil {
				log.Fatalf("failed new yandex cloud api client: %v", err)
			}
			log.Println("success updated iam token with connections")
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				updYandexClient()
			}
		}
	}()

	log.Println("prepared yandex cloud api client")

	bot, err := tgbotapi.NewBotAPI(tgBotToken)
	if err != nil {
		log.Fatalf("failed new bot api: %v", err)
	}

	log.Println("started bot")

	updates := bot.GetUpdatesChan(tgbotapi.NewUpdate(0))
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-updates:
			if err := process(ctx, bot, update.Message, yandex, mu); err != nil {
				log.Printf("failed during process message: %v\n", err)
				continue
			}
		}
	}
}

func process(ctx context.Context, bot *tgbotapi.BotAPI, tgMsg *tgbotapi.Message, yandex *yandexCloudApiClient, mu *sync.RWMutex) error {
	if tgMsg == nil {
		return fmt.Errorf("msg is nil")
	}

	mu.RLock()
	defer mu.RUnlock()

	switch {
	case tgMsg.Text == "/list":
		vms, err := yandex.listVms(ctx)
		if err != nil {
			message := tgbotapi.NewMessage(tgMsg.From.ID, err.Error())
			if _, err := bot.Send(message); err != nil {
				return fmt.Errorf("failed send message: %v", err)
			}
			return fmt.Errorf("list vms: %w", err)
		}
		vmsInfo := make([]string, 0, len(vms)+1)
		vmsInfo = append(vmsInfo, "Yandex Cloud VMs:")
		for idx, vm := range vms {
			vmsInfo = append(vmsInfo, fmt.Sprintf("%d) %s - %s, %s", idx+1, vm.GetName(), vm.GetStatus(), vm.GetId()))
		}
		text := strings.Join(vmsInfo, "\n")
		message := tgbotapi.NewMessage(tgMsg.From.ID, text)
		if _, err := bot.Send(message); err != nil {
			return fmt.Errorf("failed send message: %v", err)
		}
		return nil
	case startVmPattern.MatchString(tgMsg.Text):
		vmID := tgMsg.Text[len("start-"):]
		publicIP, err := yandex.startVm(ctx, vmID)
		if err != nil {
			message := tgbotapi.NewMessage(tgMsg.From.ID, err.Error())
			if _, err := bot.Send(message); err != nil {
				return fmt.Errorf("failed send message: %v", err)
			}
			return fmt.Errorf("failed start vm: %w", err)
		}
		message := tgbotapi.NewMessage(tgMsg.From.ID, fmt.Sprintf("started vm %s, public ip: %s", vmID, publicIP))
		if _, err := bot.Send(message); err != nil {
			return fmt.Errorf("failed send message: %v", err)
		}
		return nil
	case stopVmPattern.MatchString(tgMsg.Text):
		vmID := tgMsg.Text[len("start-"):]
		err := yandex.stopVm(ctx, vmID)
		if err != nil {
			message := tgbotapi.NewMessage(tgMsg.From.ID, err.Error())
			if _, err := bot.Send(message); err != nil {
				return fmt.Errorf("failed send message: %v", err)
			}
			return fmt.Errorf("failed stop vm: %w", err)
		}
		message := tgbotapi.NewMessage(tgMsg.From.ID, fmt.Sprintf("stopped vm %s", vmID))
		if _, err := bot.Send(message); err != nil {
			return fmt.Errorf("failed send message: %v", err)
		}
		return nil
	}

	return fmt.Errorf("unexpected command: %s", tgMsg.Text)
}

func refreshIAMToken() (string, error) {
	buf := bytes.NewBuffer(make([]byte, 0, 1000))
	errBuf := bytes.NewBuffer(make([]byte, 0, 1000))
	cmd := exec.Command("yc", "iam", "create-token")
	cmd.Stdout = buf
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cmd run: %w", err)
	}

	if errBuf.Len() != 0 {
		return "", fmt.Errorf("error during cmd run: %s", errBuf)
	}

	iamToken := buf.String()
	if iamToken == "" {
		return "", fmt.Errorf("empty iam token")
	}

	iamToken = strings.TrimSpace(iamToken)

	return iamToken, nil
}

func newYandexCloudApiClient() (*yandexCloudApiClient, error) {
	iamToken, err := refreshIAMToken()
	if err != nil {
		return nil, fmt.Errorf("refrest iam token: %w", err)
	}

	instanceServiceConn, err := grpc.Dial(
		"compute.api.cloud.yandex.net:443",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithPerRPCCredentials(yandexIAMToken(iamToken)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial instance service client: %w", err)
	}

	organizationServiceConn, err := grpc.Dial(
		"organization-manager.api.cloud.yandex.net:443",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithPerRPCCredentials(yandexIAMToken(iamToken)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial organization service client: %w", err)
	}

	resourceManagerServiceConn, err := grpc.Dial(
		"resource-manager.api.cloud.yandex.net:443",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})),
		grpc.WithPerRPCCredentials(yandexIAMToken(iamToken)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial resource manager service client: %w", err)
	}

	instanceServiceClient := compute.NewInstanceServiceClient(instanceServiceConn)
	folderServiceClient := resourcemanager.NewFolderServiceClient(resourceManagerServiceConn)
	cloudServiceClient := resourcemanager.NewCloudServiceClient(resourceManagerServiceConn)
	organizationServiceClient := organizationmanager.NewOrganizationServiceClient(organizationServiceConn)

	return &yandexCloudApiClient{
		instanceServiceClient:     instanceServiceClient,
		folderServiceClient:       folderServiceClient,
		cloudServiceClient:        cloudServiceClient,
		organizationServiceClient: organizationServiceClient,
	}, nil
}

type yandexCloudApiClient struct {
	instanceServiceClient     compute.InstanceServiceClient
	folderServiceClient       resourcemanager.FolderServiceClient
	cloudServiceClient        resourcemanager.CloudServiceClient
	organizationServiceClient organizationmanager.OrganizationServiceClient
}

func (c *yandexCloudApiClient) listVms(ctx context.Context) ([]*compute.Instance, error) {
	orgs, err := c.organizationServiceClient.List(ctx, &organizationmanager.ListOrganizationsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list org service client: %w", err)
	}

	var clouds []*resourcemanager.Cloud
	for _, org := range orgs.GetOrganizations() {
		response, err := c.cloudServiceClient.List(ctx, &resourcemanager.ListCloudsRequest{
			OrganizationId: org.GetId(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed list clouds: %w", err)
		}

		clouds = append(clouds, response.GetClouds()...)
	}

	var folders []*resourcemanager.Folder
	for _, cloud := range clouds {
		response, err := c.folderServiceClient.List(ctx, &resourcemanager.ListFoldersRequest{
			CloudId: cloud.GetId(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed list folders: %w", err)
		}

		folders = append(folders, response.GetFolders()...)
	}

	var instances []*compute.Instance
	for _, folder := range folders {
		response, err := c.instanceServiceClient.List(ctx, &compute.ListInstancesRequest{
			FolderId: folder.GetId(),
		})
		if err != nil {
			return nil, fmt.Errorf("failed list vms: %v", err)
		}
		instances = append(instances, response.GetInstances()...)
	}

	return instances, nil
}

func (c *yandexCloudApiClient) startVm(ctx context.Context, vmID string) (string, error) {
	vm, err := c.instanceServiceClient.Get(ctx, &compute.GetInstanceRequest{
		InstanceId: vmID,
	})
	if err != nil {
		return "", fmt.Errorf("failed get vm: %v", err)
	}

	if vm.GetStatus() == compute.Instance_RUNNING {
		log.Printf("vm %q already running!", vm.GetName())
		addr := getVmPublicIP(vm)
		if addr == "" {
			return "", fmt.Errorf("failed get ip, empty")
		}
		return addr, nil
	}

	_, err = c.instanceServiceClient.Start(ctx, &compute.StartInstanceRequest{
		InstanceId: vmID,
	})
	if err != nil {
		return "", fmt.Errorf("failed start vm: %v", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	for vm.GetStatus() != compute.Instance_RUNNING {
		<-ticker.C
		vm, err = c.instanceServiceClient.Get(ctx, &compute.GetInstanceRequest{
			InstanceId: vmID,
		})
		if err != nil {
			return "", fmt.Errorf("failed get vm: %v", err)
		}
	}
	ticker.Stop()

	addr := getVmPublicIP(vm)
	if addr == "" {
		return "", fmt.Errorf("failed get ip, empty")
	}

	return addr, nil
}

func (c *yandexCloudApiClient) stopVm(ctx context.Context, vmID string) error {
	vm, err := c.instanceServiceClient.Get(ctx, &compute.GetInstanceRequest{
		InstanceId: vmID,
	})
	if err != nil {
		return fmt.Errorf("failed get vm: %v", err)
	}

	if vm.GetStatus() == compute.Instance_STOPPED {
		log.Printf("vm %q already stopped!", vm.GetName())
		return nil
	}

	_, err = c.instanceServiceClient.Start(ctx, &compute.StartInstanceRequest{
		InstanceId: vmID,
	})
	if err != nil {
		return fmt.Errorf("failed start vm: %v", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	for vm.GetStatus() != compute.Instance_STOPPED {
		<-ticker.C
		vm, err = c.instanceServiceClient.Get(ctx, &compute.GetInstanceRequest{
			InstanceId: vmID,
		})
		if err != nil {
			return fmt.Errorf("failed get vm: %v", err)
		}
	}
	ticker.Stop()

	return nil
}

func getVmPublicIP(vm *compute.Instance) string {
	for _, networkInterface := range vm.GetNetworkInterfaces() {
		return networkInterface.GetPrimaryV4Address().GetOneToOneNat().GetAddress()
	}
	return ""
}

type yandexIAMToken string

func (t yandexIAMToken) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + string(t),
	}, nil
}

func (yandexIAMToken) RequireTransportSecurity() bool {
	return true
}
