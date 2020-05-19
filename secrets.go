package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/IBM-Cloud/bluemix-go"
	"github.com/IBM-Cloud/bluemix-go/api/resource/resourcev1/controller"
	"github.com/IBM-Cloud/bluemix-go/api/resource/resourcev2/managementv2"
	"github.com/IBM-Cloud/bluemix-go/models"
	"github.com/IBM-Cloud/bluemix-go/session"
)

var parser = map[string]map[string]func(creds map[string]interface{}, creds2 map[string]interface{}, cfg *Config) map[string]interface{}{
	"compliance": {
		"redis":    redisParser,
		"cloudant": cloudantParser,
	},
}

func cloudantParser(creds map[string]interface{}, creds2 map[string]interface{}, cfg *Config) map[string]interface{} {
	pass := creds["password"].(string)
	user := creds["username"].(string)
	host := creds["host"].(string)
	url := fmt.Sprintf("https://%s", host)
	if cfg.tob64 {
		pass = tob64(pass)
		user = tob64(user)
		url = tob64(url)
	}
	return map[string]interface{}{
		"cloudant_username": user,
		"cloudant_password": pass,
		"cloudant_url":      url,
	}
}

func kafkaParser(creds map[string]interface{}, creds2 map[string]interface{}, cfg *Config) map[string]interface{} {
	pass := creds["password"].(string)
	user := creds["user"].(string)
	if cfg.tob64 {
		pass = tob64(pass)
		user = tob64(user)
	}
	brokers := creds["kafka_brokers_sasl"].([]interface{})
	var brk []string

	for _, v := range brokers {
		brk = append(brk, v.(string))
	}
	//  strings.Join(brk, ",")

	return map[string]interface{}{
		"kafkaSaslUsername": user,
		"kafkaSaslPassword": pass,
	}
}

func redisParser(creds map[string]interface{}, creds2 map[string]interface{}, cfg *Config) map[string]interface{} {
	connection := creds["connection"].(map[string]interface{})
	cli := connection["cli"].(map[string]interface{})
	args := cli["arguments"].([]interface{})
	certs := cli["certificate"].(map[string]interface{})
	certBase64 := certs["certificate_base64"].(string)
	url := strings.TrimRight(args[0].([]interface{})[1].(string), "/0")
	if cfg.tob64 {
		url = tob64(url)
		certBase64 = tob64(certBase64)
	}

	return map[string]interface{}{
		"redis_url":  url,
		"redis_cert": certBase64,
	}
}

type Config struct {
	tob64        bool
	namespace    string
	inputFile    string
	outputDir    string
	apiKey       string
	outputSuffix string
	parser       string
}

//Input ...
type Input struct {
	Account string          `json:"account"`
	Region  string          `json:"region"`
	Data    RegionalDetails `json:"data"`
}

//RegionalDetails ...
type RegionalDetails struct {
	Redis    []Credentials `json:"redis,omitempty"`
	Kafka    []Credentials `json:"kafka,omitempty"`
	Cloudant []Credentials `json:"cloudant,omitempty"`
}

// Credentials ...
type Credentials struct {
	Output             string `json:"output"`
	ResourceGroup      string `json:"resource_group"`
	Name               string `json:"name"`
	PublicKeyName      string `json:"public_key_name,omitempty"`
	PrivateKeyName     string `json:"private_key_name,omitempty"`
	ResourceInstanceID string `json:"resource_instance_id,omitempty"`
}

type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}
type K8sSecret struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   Metadata               `json:"metadata"`
	Data       map[string]interface{} `json:"data"`
}

func tob64(data string) string {
	return base64.StdEncoding.EncodeToString([]byte(data))

}

func composeSecrets(credsType string, public map[string]interface{}, private map[string]interface{}, output string, cfg *Config) {
	if len(public) == 0 && len(private) == 0 {
		return
	}
	fmt.Printf("Processing %s for output %s%s\n", credsType, output, cfg.outputSuffix)
	var secret interface{}
	data := parser[cfg.parser][credsType](public, private, cfg)
	secret = K8sSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: Metadata{
			Name:      output,
			Namespace: cfg.namespace,
		},
		Data: data,
	}
	file, _ := json.MarshalIndent(secret, "", " ")
	_ = ioutil.WriteFile(cfg.outputDir+"/"+output+cfg.outputSuffix, file, 0644)
}

func fetchKeyHelper(R Credentials, rsAPI controller.ResourceServiceKeyRepository, cfg *Config) (models.ServiceKey, models.ServiceKey) {
	publicCreds := models.ServiceKey{}
	privateCreds := models.ServiceKey{}
	if R.PublicKeyName != "" {
		public, err := rsAPI.GetKeys(R.PublicKeyName)
		if err != nil {
			log.Fatal(err)
		}
		for _, pkey := range public {
			if pkey.SourceCrn.String() == R.ResourceInstanceID {
				publicCreds = pkey
				break
			}
		}
	}
	if R.PrivateKeyName != "" {
		private, err := rsAPI.GetKeys(R.PrivateKeyName)
		if err != nil {
			log.Fatal(err)
		}
		for _, pkey := range private {
			if pkey.SourceCrn.String() == R.ResourceInstanceID {
				privateCreds = pkey
				break
			}
		}
	}

	return publicCreds, privateCreds
}
func fetchKeys(regionDetails *RegionalDetails, rsAPI controller.ResourceServiceKeyRepository, cfg *Config) {
	for _, R := range regionDetails.Redis {
		publicCreds, privateCreds := fetchKeyHelper(R, rsAPI, cfg)
		composeSecrets("redis", publicCreds.Credentials, privateCreds.Credentials, R.Output, cfg)
	}

	for _, R := range regionDetails.Kafka {
		publicCreds, privateCreds := fetchKeyHelper(R, rsAPI, cfg)
		composeSecrets("kafka", publicCreds.Credentials, privateCreds.Credentials, R.Output, cfg)
	}

	for _, R := range regionDetails.Cloudant {
		publicCreds, privateCreds := fetchKeyHelper(R, rsAPI, cfg)
		composeSecrets("cloudant", publicCreds.Credentials, privateCreds.Credentials, R.Output, cfg)
	}

}

func fetchInstances(credentials *[]Credentials, resourceGroups map[string]string, rsAPI controller.ResourceServiceInstanceRepository, region string) {
	for i, R := range *credentials {
		if R.ResourceInstanceID != "" {
			continue
		}
		rsInstQuery := controller.ServiceInstanceQuery{
			Name:            R.Name,
			ResourceGroupID: resourceGroups[R.ResourceGroup],
		}
		instances, err := rsAPI.ListInstances(rsInstQuery)
		if err != nil {
			log.Fatal(err)
		}
		for _, s := range instances {
			if region == s.RegionID {
				R.ResourceInstanceID = s.Crn.String()
			}
		}
		(*credentials)[i] = R
		if R.ResourceInstanceID == "" {
			fmt.Printf("The resource %s doesn't exists in the given resource group: %s\n", R.Name, R.ResourceGroup)
		}
	}
}

func main() {
	cfg := new(Config)
	flag.BoolVar(&cfg.tob64, "tob64", false, "If provided get secrets in base64")
	flag.StringVar(&cfg.namespace, "ns", "", "Namespace")
	flag.StringVar(&cfg.inputFile, "i", "", "Input file")
	flag.StringVar(&cfg.apiKey, "apikey", "", "API Key can be exported as IC_API_KEY")
	flag.StringVar(&cfg.outputDir, "o", ".", "Input file")
	flag.StringVar(&cfg.outputSuffix, "suffix", ".json", "Suffix output files")
	flag.StringVar(&cfg.parser, "parser", "compliance", "The parser to use")

	_, err := os.Stat(cfg.outputDir)

	if os.IsNotExist(err) {
		errDir := os.MkdirAll(cfg.outputDir, 0755)
		if errDir != nil {
			log.Fatal(err)
		}
	}

	flag.Parse()

	if cfg.apiKey == "" {
		if t := os.Getenv("IC_API_KEY"); t != "" {
			cfg.apiKey = t
		}
	}
	if cfg.namespace == "" || cfg.inputFile == "" || cfg.apiKey == "" {
		fmt.Println("namespace, input file  or api key can't be empty")
		flag.PrintDefaults()
		os.Exit(1)
	}

	inputFile, err := os.Open(cfg.inputFile)
	if err != nil {
		log.Fatal(err)
	}
	i, err := ioutil.ReadAll(inputFile)
	if err != nil {
		log.Fatal(err)
	}
	var data Input
	json.Unmarshal(i, &data)
	rData := data.Data
	region := data.Region

	c := new(bluemix.Config)
	c.BluemixAPIKey = cfg.apiKey

	sess, err := session.New(c)
	if err != nil {
		log.Fatal(err)
	}

	client, err := managementv2.New(sess)
	if err != nil {
		log.Fatal(err)
	}

	resourceControllerAPI, err := controller.New(sess)
	if err != nil {
		log.Fatal(err)
	}
	rsAPI := resourceControllerAPI.ResourceServiceInstance()

	keyAPI := resourceControllerAPI.ResourceServiceKey()
	resGrpAPI := client.ResourceGroup()

	resourceGroupQuery := &managementv2.ResourceGroupQuery{
		AccountID: data.Account,
	}

	resourceGroups := make(map[string]string)
	for _, s := range rData.Redis {
		resourceGroups[s.ResourceGroup] = ""
	}
	for _, s := range rData.Kafka {
		resourceGroups[s.ResourceGroup] = ""
	}
	for _, s := range rData.Cloudant {
		resourceGroups[s.ResourceGroup] = ""
	}

	for k := range resourceGroups {
		g, err := resGrpAPI.FindByName(resourceGroupQuery, k)
		if err != nil {
			fmt.Printf("Error retrieving resource group %s : %s", k, err)
		}
		resourceGroups[k] = g[0].ID
	}

	fetchInstances(&rData.Redis, resourceGroups, rsAPI, region)
	fetchInstances(&rData.Kafka, resourceGroups, rsAPI, region)
	fetchInstances(&rData.Cloudant, resourceGroups, rsAPI, region)

	fetchKeys(&rData, keyAPI, cfg)

}
