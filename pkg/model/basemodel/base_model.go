package basemodel

import (
	"context"
	"encoding/json"

	"infer-microservices/internal"

	faiss_index "infer-microservices/internal/faiss_gogofaster"
	"infer-microservices/internal/flags"
	framework "infer-microservices/internal/tensorflow_gogofaster/core/framework"
	tfserving "infer-microservices/internal/tfserving_gogofaster"
	config_loader "infer-microservices/pkg/config_loader"
	"infer-microservices/pkg/logs"
	"infer-microservices/pkg/utils"
	"sync"
	"time"

	"github.com/allegro/bigcache"
	bloom "github.com/bits-and-blooms/bloom/v3"

	"github.com/gogo/protobuf/types"
)

var wg sync.WaitGroup
var tfservingModelVersion int64
var tfservingTimeout int64
var baseModelInstance *BaseModel
var bigCacheConfBaseModel bigcache.Config
var lifeWindowS time.Duration
var cleanWindowS time.Duration
var hardMaxCacheSize int
var maxEntrySize int
var maxEntriesInWindow int
var verbose bool
var shards int

type CreateSampleCallBackFunc func(userId string, itemList []string) (internal.ExampleFeatures, error)

var SampleCallBackFuncMap = make(map[string]CreateSampleCallBackFunc, 0)

type BaseModel struct {
	modelName       string
	serviceConfig   *config_loader.ServiceConfig
	userBloomFilter *bloom.BloomFilter
	itemBloomFilter *bloom.BloomFilter
	modelType       string //rank or recall
}

func init() {
	flagFactory := flags.FlagFactory{}
	flagTensorflow := flagFactory.CreateFlagTensorflow()
	tfservingModelVersion = *flagTensorflow.GetTfservingModelVersion()
	tfservingTimeout = *flagTensorflow.GetTfservingTimeoutMs()

	flagCache := flagFactory.CreateFlagCache()
	lifeWindowS = time.Duration(*flagCache.GetBigcacheLifeWindowS())
	cleanWindowS = time.Duration(*flagCache.GetBigcacheCleanWindowS())
	hardMaxCacheSize = *flagCache.GetBigcacheHardMaxCacheSize()
	maxEntrySize = *flagCache.GetBigcacheMaxEntrySize()
	bigCacheConfBaseModel = bigcache.Config{
		Shards:             shards,
		LifeWindow:         lifeWindowS * time.Minute,
		CleanWindow:        cleanWindowS * time.Minute,
		MaxEntriesInWindow: maxEntriesInWindow,
		MaxEntrySize:       maxEntrySize,
		Verbose:            verbose,
		HardMaxCacheSize:   hardMaxCacheSize,
		OnRemove:           nil,
		OnRemoveWithReason: nil,
	}

	//callback func config
	basemodel0 := BaseModel{}
	SampleCallBackFuncMap["recall"] = basemodel0.GetInferExampleFeaturesNotContainItems
	SampleCallBackFuncMap["rank"] = basemodel0.GetInferExampleFeaturesContainItems
}

// singleton instance
func init() {
	baseModelInstance = new(BaseModel)
}

func GetBaseModelInstance() *BaseModel {
	return baseModelInstance
}

// userid
func (b *BaseModel) SetModelName(modelName string) {
	b.modelName = modelName
}

func (b *BaseModel) GetModelName() string {
	return b.modelName
}

// serviceConfig *service_config.ServiceConfig
func (b *BaseModel) SetServiceConfig(serviceConfig *config_loader.ServiceConfig) {
	b.serviceConfig = serviceConfig
}

func (b *BaseModel) GetServiceConfig() *config_loader.ServiceConfig {
	return b.serviceConfig
}

// func (b *BaseModel) GetInferExampleFeatures() (internal.ExampleFeatures, error) {
// 	panic("please overwrite in extend models. ")

// }

func (b *BaseModel) SetUserBloomFilter(filter *bloom.BloomFilter) {
	b.userBloomFilter = filter
}

func (b *BaseModel) GetUserBloomFilter() *bloom.BloomFilter {
	return b.userBloomFilter
}

func (b *BaseModel) SetItemBloomFilter(filter *bloom.BloomFilter) {
	b.itemBloomFilter = filter
}

func (b *BaseModel) GetItemBloomFilter() *bloom.BloomFilter {
	return b.itemBloomFilter
}

// observer nontify
func (b *BaseModel) notify(sub Subject) {
	//reload baseModel
	b.SetUserBloomFilter(internal.GetUserBloomFilterInstance())
	b.SetItemBloomFilter(internal.GetItemBloomFilterInstance())
}

// Each model may have multiple ways to create samples, using callback functions to determine which method to call
func (d *BaseModel) GetInferExampleFeaturesNotContainItems(userId string, itemList []string) (internal.ExampleFeatures, error) {
	cacheKeyPrefix := userId + d.serviceConfig.GetServiceId() + d.modelName + "_samples"

	//init examples
	userExampleFeatures := &internal.SeqExampleBuff{}
	userContextExampleFeatures := &internal.SeqExampleBuff{}
	exampleData := internal.ExampleFeatures{
		UserExampleFeatures:        userExampleFeatures,
		UserContextExampleFeatures: userContextExampleFeatures,
	}

	//set cache
	bigCache, err := bigcache.NewBigCache(bigCacheConfBaseModel)
	if err != nil {
		logs.Error(err)
	}

	// if hit cacha.
	if lifeWindowS > 0 {

		//INFO:MMO, go-cache can't set MaxCacheSize. change to use bigcache.

		// if cacheData, ok := goCache.Get(cacheKeyPrefix); ok {
		// 	return cacheData.(ExampleFeatures), nil
		// }

		exampleDataBytes, _ := bigCache.Get(cacheKeyPrefix)
		err = json.Unmarshal(exampleDataBytes, &exampleData)
		if err != nil {
			logs.Error(err)
		}
		return exampleData, nil

	}

	// if not hit cache, get features from redis and request.
	userExampleFeatures, err = d.getUserExampleFeatures(userId)
	if err != nil {
		logs.Error(err)
		return exampleData, err
	}
	userContextExampleFeatures, err = d.getUserContextExampleFeatures(userId)
	if err != nil {
		logs.Error(err)
		return exampleData, err
	}

	exampleData = internal.ExampleFeatures{
		UserExampleFeatures:        userExampleFeatures,
		UserContextExampleFeatures: userContextExampleFeatures,
	}

	if lifeWindowS > 0 {
		// goCache.Set(cacheKeyPrefix, &exampleData, cacheTimeSecond)
		bigCache.Set(cacheKeyPrefix, []byte(utils.ConvertStructToJson(exampleData)))
	}

	return exampleData, nil
}

// Each model may have multiple ways to create samples, using callback functions to determine which method to call
func (d *BaseModel) GetInferExampleFeaturesContainItems(userId string, itemList []string) (internal.ExampleFeatures, error) {
	cacheKeyPrefix := userId + d.serviceConfig.GetServiceId() + d.GetModelName() + "_samples"

	//init examples
	userExampleFeatures := &internal.SeqExampleBuff{}
	userContextExampleFeatures := &internal.SeqExampleBuff{}
	itemExampleFeaturesList := make([]internal.SeqExampleBuff, 0)
	exampleData := internal.ExampleFeatures{
		UserExampleFeatures:        userExampleFeatures,
		UserContextExampleFeatures: userContextExampleFeatures,
		ItemSeqExampleFeatures:     &itemExampleFeaturesList,
	}

	//set cache
	bigCache, err := bigcache.NewBigCache(bigCacheConfBaseModel)
	if err != nil {
		return exampleData, err
	}

	// if hit cache.
	if lifeWindowS > 0 {
		exampleDataBytes, _ := bigCache.Get(cacheKeyPrefix)
		err = json.Unmarshal(exampleDataBytes, &exampleData)
		if err != nil {
			return exampleData, err
		}
		return exampleData, nil

	}

	// if not hit cache, get features from redis and request.
	userExampleFeatures, err = d.getUserExampleFeatures(userId)
	if err != nil {
		return exampleData, err
	}
	userContextExampleFeatures, err = d.getUserContextExampleFeatures(userId)
	if err != nil {
		return exampleData, err
	}

	//get items features.
	itemExampleFeaturesTmp, err := d.getItemExamplesFeatures(itemList)
	if err != nil {
		return exampleData, err
	}

	itemExampleFeaturesList = *itemExampleFeaturesTmp
	exampleData = internal.ExampleFeatures{
		UserExampleFeatures:        userExampleFeatures,
		UserContextExampleFeatures: userContextExampleFeatures,
		ItemSeqExampleFeatures:     &itemExampleFeaturesList,
	}

	if lifeWindowS > 0 {
		bigCache.Set(cacheKeyPrefix, []byte(utils.ConvertStructToJson(exampleData)))
	}

	return exampleData, nil
}

func (d *BaseModel) getItemExamplesFeatures(itemList []string) (*[]internal.SeqExampleBuff, error) {
	//TODO: use bloom filter check items, avoid all items search redis.
	redisKeyPrefix := d.serviceConfig.GetModelConfig().GetItemRedisKeyPre()
	itemSeqExampleBuffs := make([]internal.SeqExampleBuff, 0)
	for _, itemId := range itemList {
		redisKey := redisKeyPrefix + itemId
		if d.GetItemBloomFilter().Test([]byte(itemId)) {
			userExampleFeats, err := d.serviceConfig.GetRedisConfig().GetRedisPool().Get(redisKey)
			itemExampleFeatsBuff := make([]byte, 0)
			if err != nil {
				return &itemSeqExampleBuffs, nil
			} else {
				itemExampleFeatsBuff = []byte(userExampleFeats)
			}

			itemSeqExampleBuff := internal.SeqExampleBuff{
				Key:  &itemId,
				Buff: &itemExampleFeatsBuff,
			}
			itemSeqExampleBuffs = append(itemSeqExampleBuffs, itemSeqExampleBuff)
		}
	}

	return &itemSeqExampleBuffs, nil
}

// get user tfrecords offline samples
func (b *BaseModel) getUserExampleFeatures(userId string) (*internal.SeqExampleBuff, error) {
	//INFO: use bloom filter check users, avoid all users search redis.

	userSeqExampleBuff := internal.SeqExampleBuff{}
	userExampleFeatsBuff := make([]byte, 0)

	redisKey := b.serviceConfig.GetModelConfig().GetUserRedisKeyPre() + userId
	if b.userBloomFilter.Test([]byte(userId)) {
		userExampleFeats, err := b.serviceConfig.GetRedisConfig().GetRedisPool().Get(redisKey)
		if err != nil {
			//logs.Error("get item features err", err)
			return &userSeqExampleBuff, err
		} else {
			userExampleFeatsBuff = []byte(userExampleFeats) //.(string)
		}
	}

	//protrait features & realtime features.
	userSeqExampleBuff = internal.SeqExampleBuff{
		Key:  &userId,
		Buff: &userExampleFeatsBuff,
	}

	return &userSeqExampleBuff, nil
}

// get user tfrecords online samples
func (b *BaseModel) getUserContextExampleFeatures(userId string) (*internal.SeqExampleBuff, error) {
	//TODO: use bloom filter check users, avoid all users search redis.
	userContextSeqExampleBuff := internal.SeqExampleBuff{}
	userContextExampleFeatsBuff := make([]byte, 0)

	//TODO: update context features. only from requst. such as location , time
	//context features.
	userContextSeqExampleBuff = internal.SeqExampleBuff{
		Key:  &userId,
		Buff: &userContextExampleFeatsBuff,
	}

	return &userContextSeqExampleBuff, nil
}

// request tfserving service by grpc
func (b *BaseModel) RequestTfservering(userExamples *[][]byte, userContextExamples *[][]byte, itemExamples *[][]byte, tensorName string) (*[]float32, error) {
	grpcConn, err := b.serviceConfig.GetModelConfig().GetTfservingGrpcPool().Get()
	defer b.serviceConfig.GetModelConfig().GetTfservingGrpcPool().Put(grpcConn)
	if err != nil {
		return nil, err
	}

	predictConfig := tfserving.NewPredictionServiceClient(grpcConn)
	version := &types.Int64Value{Value: tfservingModelVersion}
	predictRequest := &tfserving.PredictRequest{
		ModelSpec: &tfserving.ModelSpec{
			Name:    b.serviceConfig.GetModelConfig().GetModelName(),
			Version: version,
		},
		Inputs: make(map[string]*framework.TensorProto),
	}

	//user examples
	tensorProtoUser := &framework.TensorProto{
		Dtype: framework.DataType_DT_STRING,
	}
	tensorProtoUser.TensorShape = &framework.TensorShapeProto{
		Dim: []*framework.TensorShapeProto_Dim{
			{
				Size_: int64(len(*userExamples)),
				Name:  "",
			},
		},
	}
	tensorProtoUser.StringVal = *userExamples
	predictRequest.Inputs["userExamples"] = tensorProtoUser

	//context examples, realtime
	tensorProtoUserContext := &framework.TensorProto{
		Dtype: framework.DataType_DT_STRING,
	}
	tensorProtoUserContext.TensorShape = &framework.TensorShapeProto{
		Dim: []*framework.TensorShapeProto_Dim{
			{
				Size_: int64(len(*userContextExamples)),
				Name:  "",
			},
		},
	}
	tensorProtoUserContext.StringVal = *userContextExamples
	predictRequest.Inputs["userContextExamples"] = tensorProtoUserContext

	//item examples
	tensorProtoItem := &framework.TensorProto{
		Dtype: framework.DataType_DT_STRING,
	}
	tensorProtoItem.TensorShape = &framework.TensorShapeProto{
		Dim: []*framework.TensorShapeProto_Dim{
			{
				Size_: int64(len(*itemExamples)),
				Name:  "",
			},
		},
	}
	tensorProtoItem.StringVal = *itemExamples
	predictRequest.Inputs["itemExamples"] = tensorProtoItem

	predictRequest.OutputFilter = []string{tensorName}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(tfservingTimeout)*time.Millisecond)
	defer cancel()

	predict, err := predictConfig.Predict(ctx, predictRequest)
	if err != nil {
		return nil, err
	}
	predictOut := predict.Outputs[tensorName]

	return &predictOut.FloatVal, nil
}

func (b *BaseModel) InferResultFormat(recallResult *[]*faiss_index.ItemInfo) (*[]map[string]interface{}, error) {
	recall := make([]map[string]interface{}, 0)
	recallTmp := make(chan map[string]interface{}, len(*recallResult))

	for idx := 0; idx < len(*recallResult); idx++ {
		rawCell := (*recallResult)[idx]
		wg.Add(1)
		go func(raw_cell_ *faiss_index.ItemInfo) {
			defer wg.Done()
			returnCell := make(map[string]interface{})
			returnCell["itemid"] = raw_cell_.ItemId
			returnCell["score"] = utils.FloatRound(raw_cell_.Score, 4)
			recallTmp <- returnCell
		}(rawCell)
	}
	wg.Wait()
	for idx := 0; idx < len(*recallResult); idx++ {
		returnCellTmp := <-recallTmp
		recall = append(recall, returnCellTmp)
	}
	close(recallTmp)

	return &recall, nil
}