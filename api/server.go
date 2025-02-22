package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	awsCfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"
	"github.com/redis/go-redis/v9"
	"github.com/samber/lo"
	"github.com/vmihailenco/msgpack/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"q4/adapters/oidc"
	redisAdapter "q4/adapters/redis"
	internalS3 "q4/adapters/s3"
	"q4/adapters/sse"
	"q4/api/openapi"
	"q4/models"
)

type ServerImpl struct {
	oidcProvider  *oidc.ExtendedProvider
	sseManager    sse.IConnectionManager[openapi.BidEvent]
	s3Operator    *internalS3.S3Operator
	htmlChecker   *bluemonday.Policy
	redisClient   *redis.Client
	consumer      redisAdapter.IConsumer[sse.PublishRequest[openapi.BidEvent]]
	groupConsumer redisAdapter.IGroupConsumer[BidInfo]
	wg            sync.WaitGroup
	cancelFunc    context.CancelFunc
	db            *gorm.DB

	config ServerConfig
}

func NewServer(config ServerConfig) (*ServerImpl, error) {
	const op = "NewServer"

	// 初始化OIDC提供者
	oidcProvider, err := oidc.NewExtendedProvider(config.OIDC.IssuerURL, config.OIDC.ClientID, config.OIDC.ClientSecret)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to initial OIDC provider, err=%w", op, err)
	}

	// 初始化S3客戶端
	s3Cfg, err := awsCfg.LoadDefaultConfig(
		context.Background(),
		awsCfg.WithBaseEndpoint(config.S3.Endpoint),
		awsCfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(config.S3.AccessKeyID, config.S3.SecretAccessKey, "")),
		awsCfg.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to load AWS config, err=%w", op, err)
	}
	s3Operator, err := internalS3.NewS3Operator(s3.NewFromConfig(s3Cfg), config.S3.Bucket, config.S3.PublicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to create S3 operator, err=%w", op, err)
	}

	// 初始化資料庫連線
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable&search_path=%s", config.DB.User, config.DB.Password, config.DB.Host, config.DB.Port, config.DB.Database, config.DB.Schema)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		TranslateError: true,
		NamingStrategy: schema.NamingStrategy{
			TablePrefix: config.DB.Schema + ".",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to connect to database, err=%w", op, err)
	}

	// 初始化Redis連線
	redisClient := redis.NewClient(&redis.Options{
		Addr:     config.Redis.Addr,
		Password: config.Redis.Password,
		DB:       config.Redis.DB,
	})

	// 初始化SSE管理器
	consumer, err := redisAdapter.NewConsumer(
		redisClient,
		config.Redis.StreamKeys.BidStream,
		redisAdapter.WithConsumerParseFunc(func(m map[string]any) (sse.PublishRequest[openapi.BidEvent], error) {
			bidInfo, err := redisAdapter.DefaultParseFromMessage[BidInfo](m)
			if err != nil {
				return sse.PublishRequest[openapi.BidEvent]{}, fmt.Errorf("fail to parse message to sse.PublishRequest[openapi.BidEvent], err=%w", err)
			}
			return sse.PublishRequest[openapi.BidEvent]{
				Channel: bidInfo.ItemID.String(),
				Message: openapi.BidEvent{
					Bid:  bidInfo.Amount,
					User: bidInfo.BidderID.String(),
					Time: bidInfo.CreatedAt,
				},
			}, nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to create consumer, err=%w", op, err)
	}
	sseManager, err := sse.NewConnectionManager[openapi.BidEvent](
		sse.WithLogger[openapi.BidEvent](slog.Default()),
		sse.WithSubscriber(consumer),
	)

	// 初始化group consumer
	groupConsumer, err := redisAdapter.NewGroupConsumer[BidInfo](
		redisClient,
		config.Redis.StreamKeys.BidStream,
		config.Redis.ConsumerGroup,
		config.ID,
		redisAdapter.WithGroupConsumerLogger[BidInfo](slog.Default()),
		redisAdapter.WithGroupConsumerStrictOrdering[BidInfo](true),
	)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to create group consumer, err=%w", op, err)
	}

	return &ServerImpl{
		oidcProvider:  oidcProvider,
		sseManager:    sseManager,
		s3Operator:    s3Operator,
		htmlChecker:   bluemonday.UGCPolicy(),
		redisClient:   redisClient,
		consumer:      consumer,
		groupConsumer: groupConsumer,
		db:            db,
		config:        config,
	}, nil
}

func (impl *ServerImpl) Start() {
	// 啟動consumer
	impl.consumer.Start()
	// 啟動sse connection manager
	impl.sseManager.Start()
	// 啟動group consumer
	impl.groupConsumer.Start()
	// 啟動一個worker用於將Redis中的出價紀錄存回資料庫
	ctx, cancel := context.WithCancel(context.Background())
	impl.cancelFunc = cancel
	slog.Info("Start bid synchronization worker")
	impl.wg.Add(1)
	go func() {
		logger := slog.Default().With(slog.String("caller", "BidSynchronize"))
		defer impl.wg.Done()
		defer slog.Info("Bid synchronization worker stopped")
		defer impl.groupConsumer.Close()
		ch := impl.groupConsumer.Subscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				logger.Debug("Receive message")
				handle := func() error {
					// 更新最高出價
					record := models.Bid{
						UserID:        msg.Data.BidderID,
						Amount:        msg.Data.Amount,
						AuctionItemID: msg.Data.ItemID,
					}
					auction := models.AuctionItem{ID: msg.Data.ItemID}
					if result := impl.db.Preload("CurrentBid.User").First(&auction); result.Error != nil {
						return fmt.Errorf("fail to find auction item, err=%w", result.Error)
					}
					if auction.CurrentBid != nil && auction.CurrentBid.Amount < msg.Data.Amount || auction.CurrentBid == nil && auction.StartingPrice < msg.Data.Amount {
						logger.Debug("Update current bid", slog.String("itemID", msg.Data.ItemID.String()), slog.Int64("from", int64(auction.CurrentBid.Amount)), slog.Int64("to", int64(msg.Data.Amount)))
						auction.CurrentBidID = &record.ID
						auction.CurrentBid = &record
						if result := impl.db.Save(&auction); result.Error != nil {
							return fmt.Errorf("fail to update auction item, err=%w", result.Error)
						}
					} else {
						logger.Warn("Ignore lower bid", slog.String("itemID", msg.Data.ItemID.String()), slog.Int64("current", int64(auction.CurrentBid.Amount)), slog.Int64("new", int64(msg.Data.Amount)))
					}
					return nil
				}
				handleErr := handle()
				if handleErr != nil {
					logger.Error("Fail to synchronize bid", slog.Any("error", handleErr))
					if err := msg.Fail(ctx, handleErr); err != nil {
						logger.Error("Fail to fail message", slog.Any("error", err))
					}
					continue
				}
				if err := msg.Done(ctx); err != nil {
					logger.Error("Sync success but fail to done message", slog.Any("error", err))
					if err := msg.Fail(ctx, err); err != nil {
						logger.Error("Sync success but fail to fail message", slog.Any("error", err))
					}
					continue
				}
				logger.Debug("Synchronize success")
			}
		}
	}()
}

func (impl *ServerImpl) Close() {
	// 關閉group consumer
	impl.groupConsumer.Close()
	// 關閉worker
	impl.cancelFunc()
	impl.wg.Wait()
	// 關閉consumer
	impl.consumer.Close()
	// 關閉sse connection manager
	impl.sseManager.Done()
}

// Add a new auction item
// (POST /auction/item)
func (impl *ServerImpl) PostAuctionItem(ctx context.Context, request openapi.PostAuctionItemRequestObject) (openapi.PostAuctionItemResponseObject, error) {
	const op = "PostAuctionItem"
	// 檢查拍賣物品的拍賣時間和結束時間是否合法
	if request.Body.StartTime.After(request.Body.EndTime) || request.Body.EndTime.Before(time.Now()) {
		return openapi.PostAuctionItem400JSONResponse{
			Message: lo.ToPtr("Invalid auction time"),
		}, nil
	}
	// 檢查使用者是否有權限新增拍賣物品
	if request.Params.AccessToken == nil {
		return openapi.PostAuctionItem401Response{}, nil
	}
	user, err := impl.oidcProvider.Introspect(*request.Params.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to introspect token, err=%w", op, err)
	}
	if !user.Active {
		return openapi.PostAuctionItem401Response{}, nil
	}
	// 處理拍賣描述
	if request.Body.Description != nil {
		request.Body.Description = lo.ToPtr(impl.htmlChecker.Sanitize(*request.Body.Description))
	}
	// 處理預設值
	if request.Body.Description == nil {
		request.Body.Description = lo.ToPtr("")
	}
	if request.Body.StartingPrice == nil {
		request.Body.StartingPrice = lo.ToPtr(int64(0))
	}
	if request.Body.StartTime == nil {
		request.Body.StartTime = lo.ToPtr(time.Now())
	}
	if request.Body.Carousels == nil {
		request.Body.Carousels = lo.ToPtr([]string{})
	}
	// 儲存拍賣物品
	dbUser := models.User{Username: user.Name}
	if result := impl.db.First(&dbUser); result.Error != nil {
		return nil, fmt.Errorf("[%s] Fail to find user, err=%w", op, result.Error)
	}
	auction := models.AuctionItem{
		UserID:        dbUser.ID,
		Title:         request.Body.Title,
		Description:   *request.Body.Description,
		StartingPrice: uint32(*request.Body.StartingPrice),
		CurrentBidID:  nil,
		StartTime:     *request.Body.StartTime,
		EndTime:       request.Body.EndTime,
		Carousels:     *request.Body.Carousels,
	}
	if result := impl.db.Debug().Create(&auction); result.Error != nil {
		return nil, fmt.Errorf("[%s] Fail to create auction item, err=%w", op, result.Error)
	}
	return openapi.PostAuctionItem201Response{
		Headers: openapi.PostAuctionItem201ResponseHeaders{
			Location: auction.ID.String(),
		},
	}, nil
}

// Get auction item details
// (GET /auction/item/{itemID})
func (impl *ServerImpl) GetAuctionItemItemID(ctx context.Context, request openapi.GetAuctionItemItemIDRequestObject) (openapi.GetAuctionItemItemIDResponseObject, error) {
	const op = "GetAuctionItemItemID"
	// 檢查拍賣物品是否存在
	auction := models.AuctionItem{ID: request.ItemID}
	if result := impl.db.Debug().
		Preload(
			"BidRecords",
			func(db *gorm.DB) *gorm.DB {
				return db.Order((clause.OrderByColumn{Column: clause.Column{Name: "created_at"}, Desc: true}))
			}).
		Preload("BidRecords.User").
		Preload("CurrentBid.User").
		First(&auction); result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return openapi.GetAuctionItemItemID404Response{}, nil
		}
		return nil, fmt.Errorf("[%s] Fail to find auction item, err=%w", op, result.Error)
	}
	// 取得所有出價紀錄
	bidRecords := make([]openapi.BidEvent, len(auction.BidRecords))
	for i, bid := range auction.BidRecords {
		bidRecords[i] = openapi.BidEvent{
			Bid:  bid.Amount,
			User: bid.User.Username,
			Time: bid.CreatedAt,
		}
	}

	// 回傳拍賣物品資訊
	return openapi.GetAuctionItemItemID200JSONResponse{
		BidRecords:  bidRecords,
		Description: auction.Description,
		EndTime:     auction.EndTime,
		Title:       auction.Title,
		StartPrice:  int64(auction.StartingPrice),
		StartTime:   auction.StartTime,
		Carousels:   auction.Carousels,
	}, nil
}

// Place a bid on an auction item
// (POST /auction/item/{itemID}/bids)
func (impl *ServerImpl) PostAuctionItemItemIDBids(ctx context.Context, request openapi.PostAuctionItemItemIDBidsRequestObject) (openapi.PostAuctionItemItemIDBidsResponseObject, error) {
	const op = "PostAuctionItemItemIDBids"
	// 檢查拍賣物品是否存在
	auction := models.AuctionItem{ID: request.ItemID}
	if result := impl.db.Preload("CurrentBid.User").First(&auction); result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return openapi.PostAuctionItemItemIDBids400JSONResponse{}, nil
		}
		return nil, fmt.Errorf("[%s] Fail to find auction item, err=%w", op, result.Error)
	}
	// 檢查拍賣物品是否已經開始
	if time.Now().Before(auction.StartTime) {
		return openapi.PostAuctionItemItemIDBids403JSONResponse{}, nil
	}
	// 檢查拍賣物品是否已經結束
	if time.Now().After(auction.EndTime) {
		return openapi.PostAuctionItemItemIDBids410JSONResponse{}, nil
	}
	// 檢查使用者是否可以出價
	if request.Params.AccessToken == nil {
		return openapi.PostAuctionItemItemIDBids401Response{}, nil
	}
	user, err := impl.oidcProvider.Introspect(*request.Params.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to introspect token, err=%w", op, err)
	}
	if !user.Active {
		return openapi.PostAuctionItemItemIDBids401Response{}, nil
	}
	dbUser := models.User{Username: user.Name}
	if result := impl.db.First(&dbUser); result.Error != nil {
		return nil, fmt.Errorf("[%s] Fail to find user, err=%w", op, result.Error)
	}

	// 取得Redis上商品的出價鎖
	lockKey := fmt.Sprintf("%sauction:%s:lock", impl.config.Redis.KeyPrefix, request.ItemID)
	dMutex := redisAdapter.NewAutoRenewMutex(impl.redisClient, lockKey)
	if err := dMutex.Lock(ctx); err != nil {
		return nil, fmt.Errorf("[%s] Fail to acquire bid lock, err=%w", op, err)
	}
	defer func() {
		_, err := dMutex.Unlock()
		if err != nil {
			slog.Warn("[%s] Fail to release bid lock, err=%w", op, err)
		}
	}()

	// 準備出價資訊
	auctionKey := fmt.Sprintf("%sauction:%s", impl.config.Redis.KeyPrefix, request.ItemID)
	bidInfo := BidInfo{
		ItemID:    request.ItemID,
		BidderID:  dbUser.ID,
		Amount:    request.Body.Bid,
		CreatedAt: time.Now(),
	}
	bidInfoBytes, err := msgpack.Marshal(bidInfo)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to marshal bid info, err=%w", op, err)
	}
	bidInfoBase64 := base64.StdEncoding.EncodeToString(bidInfoBytes)
	// 透過Lua script來處理出價
	status, err := BidScript.Run(ctx, impl.redisClient, []string{auctionKey, impl.config.Redis.StreamKeys.BidStream}, request.Body.Bid, bidInfoBase64).Int()
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to place bid, err=%w", op, err)
	}
	if status == 0 {
		return openapi.PostAuctionItemItemIDBids400JSONResponse{}, nil
	} else if status == 1 {
		slog.Info("Higher bid occurs", slog.String("user", dbUser.ID.String()), slog.Int64("bid", int64(request.Body.Bid)), slog.String("auctionID", dbUser.ID.String()))
		return openapi.PostAuctionItemItemIDBids200Response{}, nil
	} else if status != -1 {
		return nil, fmt.Errorf("[%s] Invalid script return value: %d", op, status)
	}

	// 將資料庫紀錄的最高出價寫入Redis
	// NOTE: 由於每次出價都一定會更新Redis，所以除非從請求剛進來時系統向資料庫請求拍賣資訊，
	//       到取得鎖的過程中，拍賣物品的最高出價已經被其他人更新，且Redis的資料也過期，不然
	//       請求剛進來時系統向資料庫請求拍賣資訊都能確定是最新的。
	currentBid := auction.StartingPrice
	if auction.CurrentBidID != nil {
		currentBid = auction.CurrentBid.Amount
	}
	if err := impl.redisClient.Set(ctx, auctionKey, currentBid, impl.config.Redis.ExpireTime).Err(); err != nil {
		return nil, fmt.Errorf("[%s] Fail to update current bid in Redis, err=%w", op, err)
	}

	// 再次透過Lua script來處理出價
	status, err = BidScript.Run(ctx, impl.redisClient, []string{auctionKey, impl.config.Redis.StreamKeys.BidStream}, request.Body.Bid, bidInfoBase64).Int()
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to place bid, err=%w", op, err)
	}
	if status == 0 {
		return openapi.PostAuctionItemItemIDBids400JSONResponse{}, nil
	} else if status == 1 {
		slog.Info("Higher bid occurs", slog.String("user", dbUser.ID.String()), slog.Int64("bid", int64(request.Body.Bid)), slog.String("auctionID", dbUser.ID.String()))
		return openapi.PostAuctionItemItemIDBids200Response{}, nil
	} else if status != -1 {
		return nil, fmt.Errorf("[%s] Invalid script return value: %d", op, status)
	}
	return nil, fmt.Errorf("[%s] Impossible case occurs: %d", op, status)
}

// Track auction item events
// (GET /auction/item/{itemID}/events)
func (impl *ServerImpl) GetAuctionItemItemIDEvents(ctx context.Context, request openapi.GetAuctionItemItemIDEventsRequestObject) (openapi.GetAuctionItemItemIDEventsResponseObject, error) {
	const op = "GetAuctionItemItemIDEvents"
	// 檢查拍賣物品是否存在
	auction := models.AuctionItem{ID: request.ItemID}
	if result := impl.db.First(&auction); result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return openapi.GetAuctionItemItemIDEvents404Response{}, nil
		}
		return nil, fmt.Errorf("[%s] Fail to find auction item, err=%w", op, result.Error)
	}
	// 檢查拍賣物品是否已經開始拍賣(開始前5分鐘開放連線)
	if time.Now().Before(auction.StartTime.Add(-5 * time.Minute)) {
		return openapi.GetAuctionItemItemIDEvents403JSONResponse{
			Message: lo.ToPtr("Auction has not started"),
		}, nil
	}
	// 檢查拍賣物品是否已經結束拍賣
	if time.Now().After(auction.EndTime) {
		return openapi.GetAuctionItemItemIDEvents410JSONResponse{
			Message: lo.ToPtr("Auction has ended"),
		}, nil
	}
	// SSE請求合法，開始初始化串流
	c := ctx.(*gin.Context)
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")
	ch, err := impl.sseManager.Subscribe(request.ItemID.String())
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to subscribe to item events, err=%w", op, err)
	}
LOOP:
	for {
		select {
		case <-w.CloseNotify():
			impl.sseManager.Unsubscribe(request.ItemID.String(), ch)
			break LOOP
		case event := <-ch:
			c.SSEvent("bid", event)
			w.Flush()
		// 30秒沒有事件就發送一個空行，確保瀏覽器和Cloudflare不會斷開連線
		case <-time.After(30 * time.Second):
			w.WriteString("\n\n")
			w.Flush()
		}
	}
	return openapi.GetAuctionItemItemIDEvents200Response{}, nil
}

// List auction items
// (GET /auction/items)
func (impl *ServerImpl) GetAuctionItems(ctx context.Context, request openapi.GetAuctionItemsRequestObject) (openapi.GetAuctionItemsResponseObject, error) {
	const op = "GetAuctionItems"
	now := time.Now()
	// 建立查詢
	query := impl.db.Debug().Joins("CurrentBid").Model(&models.AuctionItem{})
	//  - title
	if request.Params.Title != nil {
		query = query.Where("title LIKE ?", "%"+*request.Params.Title+"%")
	}
	//  - start_price
	if request.Params.StartPrice != nil {
		if request.Params.StartPrice.From != nil {
			query = query.Where("starting_price >= ?", *request.Params.StartPrice.From)
		}
		if request.Params.StartPrice.To != nil {
			query = query.Where("starting_price <= ?", *request.Params.StartPrice.To)
		}
	}
	//  - start_time
	if request.Params.StartTime != nil {
		if request.Params.StartTime.From != nil {
			query = query.Where("start_time >= ?", *request.Params.StartTime.From)
		}
		if request.Params.StartTime.To != nil {
			query = query.Where("start_time <= ?", *request.Params.StartTime.To)
		}
	}
	//  - end_time
	if request.Params.EndTime != nil {
		if request.Params.EndTime.From != nil {
			query = query.Where("end_time >= ?", *request.Params.EndTime.From)
		}
		if request.Params.EndTime.To != nil {
			query = query.Where("end_time <= ?", *request.Params.EndTime.To)
		}
	}
	//  - current_bid
	// 目前實際價格是記錄在另外一張表(bids)中，所以需要透過join來查詢
	// 且如果目前沒有人出價，則需要使用起標價格來進行篩選
	if request.Params.CurrentBid != nil {
		if request.Params.CurrentBid.From != nil {
			query = query.Where(`"CurrentBid".amount >= ? OR current_bid_id IS NULL AND starting_price >= ?`, *request.Params.CurrentBid.From, *request.Params.CurrentBid.From)
		}
		if request.Params.CurrentBid.To != nil {
			query = query.Where(`"CurrentBid".amount <= ? OR current_bid_id IS NULL AND starting_price <= ?`, *request.Params.CurrentBid.To, *request.Params.CurrentBid.To)
		}
	}
	//  - sort
	sortKey, desc := "title", false
	if request.Params.Sort != nil {
		if request.Params.Sort.Key != nil {
			switch *request.Params.Sort.Key {
			case openapi.Title:
				sortKey = "title"
			case openapi.StartTime:
				sortKey = "start_time"
			case openapi.EndTime:
				sortKey = "end_time"
			case openapi.CurrentBid:
				sortKey = "current_price"
			case openapi.StartPrice:
				sortKey = "starting_price"
			default:
				return openapi.GetAuctionItems400JSONResponse{
					Message: lo.ToPtr("Invalid sort key"),
				}, nil
			}
		}
		if request.Params.Sort.Order != nil {
			desc = *request.Params.Sort.Order == openapi.Desc
		}
	}
	query = query.Order(clause.OrderBy{Columns: []clause.OrderByColumn{
		{Column: clause.Column{Name: sortKey}, Desc: desc},
		{Column: clause.Column{Name: "id"}, Desc: false},
	}})
	//  - cursor
	if request.Params.LastItemID != nil {
		var cursor string
		if result := impl.db.Model(&models.AuctionItem{}).Select(sortKey).Where("id = ?", *request.Params.LastItemID).First(&cursor); result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return openapi.GetAuctionItems400JSONResponse{
					Message: lo.ToPtr("Last item not found"),
				}, nil
			}
			return nil, fmt.Errorf("[%s] Fail to find last item, err=%w", op, result.Error)
		}
		if desc {
			query = query.Where(sortKey+" < ?", cursor)
		} else {
			query = query.Where(sortKey+" > ?", cursor)
		}
		query = query.Or(sortKey+" = ? AND id > ?", cursor, *request.Params.LastItemID)
	}
	//  - size
	size := uint32(1)
	if request.Params.Size != nil {
		size = *request.Params.Size
	}
	query = query.Limit(int(size))
	//  - excludeEnded
	if request.Params.ExcludeEnded != nil && *request.Params.ExcludeEnded {
		query = query.Where("end_time > ?", now)
	}
	// todo: 嘗試從redis查詢，如果有就直接返回redis內儲存的查詢結果
	// 查詢拍賣物品
	var auctions []models.AuctionItem
	if result := query.Find(&auctions); result.Error != nil {
		return nil, fmt.Errorf("[%s] Fail to list auction items, err=%w", op, result.Error)
	}
	if len(auctions) == 0 {
		return openapi.GetAuctionItems404Response{}, nil
	}
	output := make([]struct {
		CurrentBid uint32    `json:"currentBid"`
		EndTime    time.Time `json:"endTime"`
		Id         uuid.UUID `json:"id"`
		IsEnded    bool      `json:"isEnded"`
		StartTime  time.Time `json:"startTime"`
		Title      string    `json:"title"`
	}, len(auctions))
	for i, auction := range auctions {
		if auction.CurrentBid != nil {
			output[i].CurrentBid = uint32(auction.CurrentBid.Amount)
		} else {
			output[i].CurrentBid = uint32(auction.StartingPrice)
		}
		output[i].Id = auction.ID
		output[i].Title = auction.Title
		output[i].EndTime = auction.EndTime
		output[i].StartTime = auction.StartTime
		output[i].IsEnded = now.After(auction.EndTime)
	}
	return openapi.GetAuctionItems200JSONResponse{
		Count: len(auctions),
		Items: output,
	}, nil
}

// Exchange authorization code
// (GET /auth/callback)
func (impl *ServerImpl) GetAuthCallback(ctx context.Context, request openapi.GetAuthCallbackRequestObject) (openapi.GetAuthCallbackResponseObject, error) {
	const op = "GetAuthCallback"
	// 驗證callback的參數和login時產生的參數是否相同
	reqestState, requestNonce := "", ""
	if request.Params.RequestState != nil {
		reqestState = *request.Params.RequestState
	}
	if request.Params.RequestNonce != nil {
		requestNonce = *request.Params.RequestNonce
	}
	verifier := impl.oidcProvider.NewExchangeVerifier(reqestState, requestNonce)
	// 向驗證伺服器交換token
	token, err := impl.oidcProvider.Exchange(ctx, verifier, request.Params.Code, request.Params.State, request.Params.RedirectUrl)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to exchange token, err=%w", op, err)
	}
	// 從id token中取得使用者資料
	var idTokenClaims oidc.UserInfo
	if err := json.Unmarshal(*token.IDTokenClaims, &idTokenClaims); err != nil {
		return nil, fmt.Errorf("[%s] Fail to retrieve email from id token, err=%w", op, err)
	}
	// 建立使用者資料(用於關聯使用者操作)
	var user models.User
	result := impl.db.FirstOrCreate(&user, &models.User{Username: idTokenClaims.Name})
	if result.Error != nil {
		return nil, fmt.Errorf("[%s] Fail to create user, err=%w", op, result.Error)
	}
	// 設定cookie並導向
	redirectUrl, err := url.Parse(request.Params.RedirectUrl)
	if err != nil {
		slog.Warn("[%s] Bad redirect url, err=%w", op, err)
	}
	return openapi.GetAuthCallback200Response{
		Headers: openapi.GetAuthCallback200ResponseHeaders{
			Location: redirectUrl.Query().Get("redirect_url"),
			SetCookieAccessTokenHttpOnlySecureMaxAge10800: token.OAuth2Token.AccessToken,
			SetCookieUsernameMaxAge10800:                  idTokenClaims.Nickname,
		},
	}, nil
}

// Obtain authentication url
// (GET /auth/login)
func (impl *ServerImpl) GetAuthLogin(ctx context.Context, request openapi.GetAuthLoginRequestObject) (openapi.GetAuthLoginResponseObject, error) {
	const op = "GetAuthLogin"
	state, err := generateID("st")
	if err != nil {
		return nil, fmt.Errorf("[%s] Unable to generate state, err=%w", op, err)
	}
	nonce, err := generateID("n")
	if err != nil {
		return nil, fmt.Errorf("[%s] Unable to generate nonce, err=%w", op, err)
	}
	return openapi.GetAuthLogin200Response{
		Headers: openapi.GetAuthLogin200ResponseHeaders{
			Location: impl.oidcProvider.AuthURL(state, nonce, request.Params.RedirectUrl, []string{"email", "openid", "profile"}),
			SetCookieRequestStateHttpOnlySecureMaxAge120: state,
			SetCookieRequestNonceHttpOnlySecureMaxAge120: nonce,
		},
	}, nil
}

// Revoke authentication token
// (GET /auth/logout)
func (impl *ServerImpl) GetAuthLogout(ctx context.Context, request openapi.GetAuthLogoutRequestObject) (openapi.GetAuthLogoutResponseObject, error) {
	// only clear the cookie without revoking the token
	return openapi.GetAuthLogout200Response{}, nil
}

// Upload an image
// (POST /image)
func (impl *ServerImpl) PostImage(ctx context.Context, request openapi.PostImageRequestObject) (openapi.PostImageResponseObject, error) {
	const op = "PostImage"
	// 檢查使用者是否可以上傳圖片
	if request.Params.AccessToken == nil {
		return openapi.PostImage401Response{}, nil
	}
	user, err := impl.oidcProvider.Introspect(*request.Params.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to introspect token, err=%w", op, err)
	}
	if !user.Active {
		return openapi.PostImage401Response{}, nil
	}
	// 限制圖片
	// 	1. 小於5MB
	// 	2. MIME類型為不包含腳本的圖片檔案
	body := internalS3.NewMaxSizeReader(request.Body, 5<<20)
	file, err := io.ReadAll(body)
	if errors.As(err, &internalS3.ErrReachLimitType) {
		return openapi.PostImage400JSONResponse{
			Message: lo.ToPtr(err.Error()),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to read image, err=%w", op, err)
	}
	mimeType := http.DetectContentType(file)
	secure, ext := internalS3.CheckSecureImageAndGetExtension(mimeType)
	if !secure {
		return openapi.PostImage400JSONResponse{
			Message: lo.ToPtr(fmt.Sprintf("Invalid image type: %s", mimeType)),
		}, nil
	}
	// 透過S3 API儲存圖片
	url, err := impl.s3Operator.UploadFileToS3(ctx, uuid.New().String()+"."+ext, mimeType, file)
	if err != nil {
		return nil, fmt.Errorf("[%s] Fail to upload image, err=%w", op, err)
	}

	return openapi.PostImage201Response{
		Headers: openapi.PostImage201ResponseHeaders{
			Location: url,
		},
	}, nil
}

func generateID(prefix string) (string, error) {
	const op = "generateID"
	bytes := make([]byte, 20)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("[%s] Fail to generate unique id, err=%w", op, err)
	}
	return prefix + "_" + base64.URLEncoding.EncodeToString(bytes), nil
}
