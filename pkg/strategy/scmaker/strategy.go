package scmaker

import (
	"context"
	"fmt"
	"math"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/indicator"
	"github.com/c9s/bbgo/pkg/types"
)

const ID = "scmaker"

var ten = fixedpoint.NewFromInt(10)

type advancedOrderCancelApi interface {
	CancelAllOrders(ctx context.Context) ([]types.Order, error)
	CancelOrdersBySymbol(ctx context.Context, symbol string) ([]types.Order, error)
}

type BollingerConfig struct {
	Interval types.Interval `json:"interval"`
	Window   int            `json:"window"`
	K        float64        `json:"k"`
}

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

// Strategy scmaker is a stable coin market maker
type Strategy struct {
	Environment *bbgo.Environment
	Market      types.Market

	Symbol string `json:"symbol"`

	NumOfLiquidityLayers int `json:"numOfLiquidityLayers"`

	LiquidityUpdateInterval types.Interval   `json:"liquidityUpdateInterval"`
	PriceRangeBollinger     *BollingerConfig `json:"priceRangeBollinger"`
	StrengthInterval        types.Interval   `json:"strengthInterval"`

	AdjustmentUpdateInterval types.Interval `json:"adjustmentUpdateInterval"`

	MidPriceEMA            *types.IntervalWindow `json:"midPriceEMA"`
	LiquiditySlideRule     *bbgo.SlideRule       `json:"liquidityScale"`
	LiquidityLayerTickSize fixedpoint.Value      `json:"liquidityLayerTickSize"`

	MaxExposure fixedpoint.Value `json:"maxExposure"`

	MinProfit fixedpoint.Value `json:"minProfit"`

	Position    *types.Position    `json:"position,omitempty" persistence:"position"`
	ProfitStats *types.ProfitStats `json:"profitStats,omitempty" persistence:"profit_stats"`

	session                                 *bbgo.ExchangeSession
	orderExecutor                           *bbgo.GeneralOrderExecutor
	liquidityOrderBook, adjustmentOrderBook *bbgo.ActiveOrderBook
	book                                    *types.StreamOrderBook

	liquidityScale bbgo.Scale

	// indicators
	ewma      *indicator.EWMAStream
	boll      *indicator.BOLLStream
	intensity *IntensityStream
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s", ID, s.Symbol)
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	session.Subscribe(types.BookChannel, s.Symbol, types.SubscribeOptions{})
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.AdjustmentUpdateInterval})
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.LiquidityUpdateInterval})

	if s.MidPriceEMA != nil {
		session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.MidPriceEMA.Interval})
	}
}

func (s *Strategy) Run(ctx context.Context, orderExecutor bbgo.OrderExecutor, session *bbgo.ExchangeSession) error {
	instanceID := s.InstanceID()

	s.session = session
	s.book = types.NewStreamBook(s.Symbol)
	s.book.BindStream(session.UserDataStream)

	s.liquidityOrderBook = bbgo.NewActiveOrderBook(s.Symbol)
	s.liquidityOrderBook.BindStream(session.UserDataStream)

	s.adjustmentOrderBook = bbgo.NewActiveOrderBook(s.Symbol)
	s.adjustmentOrderBook.BindStream(session.UserDataStream)

	// If position is nil, we need to allocate a new position for calculation
	if s.Position == nil {
		s.Position = types.NewPositionFromMarket(s.Market)
	}

	// Always update the position fields
	s.Position.Strategy = ID
	s.Position.StrategyInstanceID = instanceID

	// if anyone of the fee rate is defined, this assumes that both are defined.
	// so that zero maker fee could be applied
	if s.session.MakerFeeRate.Sign() > 0 || s.session.TakerFeeRate.Sign() > 0 {
		s.Position.SetExchangeFeeRate(s.session.ExchangeName, types.ExchangeFee{
			MakerFeeRate: s.session.MakerFeeRate,
			TakerFeeRate: s.session.TakerFeeRate,
		})
	}

	if s.ProfitStats == nil {
		s.ProfitStats = types.NewProfitStats(s.Market)
	}

	scale, err := s.LiquiditySlideRule.Scale()
	if err != nil {
		return err
	}

	if err := scale.Solve(); err != nil {
		return err
	}

	if cancelApi, ok := session.Exchange.(advancedOrderCancelApi); ok {
		_, _ = cancelApi.CancelOrdersBySymbol(ctx, s.Symbol)
	}

	s.liquidityScale = scale

	s.orderExecutor = bbgo.NewGeneralOrderExecutor(session, s.Symbol, ID, instanceID, s.Position)
	s.orderExecutor.BindEnvironment(s.Environment)
	s.orderExecutor.BindProfitStats(s.ProfitStats)
	s.orderExecutor.Bind()
	s.orderExecutor.TradeCollector().OnPositionUpdate(func(position *types.Position) {
		bbgo.Sync(ctx, s)
	})

	s.initializeMidPriceEMA(session)
	s.initializePriceRangeBollinger(session)
	s.initializeIntensityIndicator(session)

	session.UserDataStream.OnStart(func() {
		s.placeLiquidityOrders(ctx)
	})

	session.MarketDataStream.OnKLineClosed(func(k types.KLine) {
		if k.Interval == s.AdjustmentUpdateInterval {
			s.placeAdjustmentOrders(ctx)
		}

		if k.Interval == s.LiquidityUpdateInterval {
			s.placeLiquidityOrders(ctx)
		}
	})

	bbgo.OnShutdown(ctx, func(ctx context.Context, wg *sync.WaitGroup) {
		defer wg.Done()

		err := s.liquidityOrderBook.GracefulCancel(ctx, s.session.Exchange)
		logErr(err, "unable to cancel liquidity orders")

		err = s.adjustmentOrderBook.GracefulCancel(ctx, s.session.Exchange)
		logErr(err, "unable to cancel adjustment orders")
	})

	return nil
}

func (s *Strategy) preloadKLines(inc *indicator.KLineStream, session *bbgo.ExchangeSession, symbol string, interval types.Interval) {
	if store, ok := session.MarketDataStore(symbol); ok {
		if kLinesData, ok := store.KLinesOfInterval(interval); ok {
			for _, k := range *kLinesData {
				inc.EmitUpdate(k)
			}
		}
	}
}

func (s *Strategy) initializeMidPriceEMA(session *bbgo.ExchangeSession) {
	kLines := indicator.KLines(session.MarketDataStream, s.Symbol, s.MidPriceEMA.Interval)
	s.ewma = indicator.EWMA2(indicator.ClosePrices(kLines), s.MidPriceEMA.Window)

	s.preloadKLines(kLines, session, s.Symbol, s.MidPriceEMA.Interval)
}

func (s *Strategy) initializeIntensityIndicator(session *bbgo.ExchangeSession) {
	kLines := indicator.KLines(session.MarketDataStream, s.Symbol, s.StrengthInterval)
	s.intensity = Intensity(kLines, 10)

	s.preloadKLines(kLines, session, s.Symbol, s.StrengthInterval)
}

func (s *Strategy) initializePriceRangeBollinger(session *bbgo.ExchangeSession) {
	kLines := indicator.KLines(session.MarketDataStream, s.Symbol, s.PriceRangeBollinger.Interval)
	closePrices := indicator.ClosePrices(kLines)
	s.boll = indicator.BOLL2(closePrices, s.PriceRangeBollinger.Window, s.PriceRangeBollinger.K)

	s.preloadKLines(kLines, session, s.Symbol, s.PriceRangeBollinger.Interval)
}

func (s *Strategy) placeAdjustmentOrders(ctx context.Context) {
	_ = s.adjustmentOrderBook.GracefulCancel(ctx, s.session.Exchange)

	if s.Position.IsDust() {
		return
	}

	ticker, err := s.session.Exchange.QueryTicker(ctx, s.Symbol)
	if logErr(err, "unable to query ticker") {
		return
	}

	if _, err := s.session.UpdateAccount(ctx); err != nil {
		logErr(err, "unable to update account")
		return
	}

	baseBal, _ := s.session.Account.Balance(s.Market.BaseCurrency)
	quoteBal, _ := s.session.Account.Balance(s.Market.QuoteCurrency)

	var adjOrders []types.SubmitOrder

	posSize := s.Position.Base.Abs()
	tickSize := s.Market.TickSize

	if s.Position.IsShort() {
		price := profitProtectedPrice(types.SideTypeBuy, s.Position.AverageCost, ticker.Sell.Add(tickSize.Neg()), s.session.MakerFeeRate, s.MinProfit)
		quoteQuantity := fixedpoint.Min(price.Mul(posSize), quoteBal.Available)
		bidQuantity := quoteQuantity.Div(price)

		if s.Market.IsDustQuantity(bidQuantity, price) {
			return
		}

		adjOrders = append(adjOrders, types.SubmitOrder{
			Symbol:      s.Symbol,
			Type:        types.OrderTypeLimitMaker,
			Side:        types.SideTypeBuy,
			Price:       price,
			Quantity:    bidQuantity,
			Market:      s.Market,
			TimeInForce: types.TimeInForceGTC,
		})
	} else if s.Position.IsLong() {
		price := profitProtectedPrice(types.SideTypeSell, s.Position.AverageCost, ticker.Buy.Add(tickSize), s.session.MakerFeeRate, s.MinProfit)
		askQuantity := fixedpoint.Min(posSize, baseBal.Available)

		if s.Market.IsDustQuantity(askQuantity, price) {
			return
		}

		adjOrders = append(adjOrders, types.SubmitOrder{
			Symbol:      s.Symbol,
			Type:        types.OrderTypeLimitMaker,
			Side:        types.SideTypeSell,
			Price:       price,
			Quantity:    askQuantity,
			Market:      s.Market,
			TimeInForce: types.TimeInForceGTC,
		})
	}

	createdOrders, err := s.orderExecutor.SubmitOrders(ctx, adjOrders...)
	if logErr(err, "unable to place liquidity orders") {
		return
	}

	s.adjustmentOrderBook.Add(createdOrders...)
}

func (s *Strategy) placeLiquidityOrders(ctx context.Context) {
	err := s.liquidityOrderBook.GracefulCancel(ctx, s.session.Exchange)
	if logErr(err, "unable to cancel orders") {
		return
	}

	ticker, err := s.session.Exchange.QueryTicker(ctx, s.Symbol)
	if logErr(err, "unable to query ticker") {
		return
	}

	if _, err := s.session.UpdateAccount(ctx); err != nil {
		logErr(err, "unable to update account")
		return
	}

	baseBal, _ := s.session.Account.Balance(s.Market.BaseCurrency)
	quoteBal, _ := s.session.Account.Balance(s.Market.QuoteCurrency)

	spread := ticker.Sell.Sub(ticker.Buy)
	tickSize := fixedpoint.Max(s.LiquidityLayerTickSize, s.Market.TickSize)

	midPriceEMA := s.ewma.Last(0)
	midPrice := fixedpoint.NewFromFloat(midPriceEMA)

	bandWidth := s.boll.Last(0)

	log.Infof("spread: %f mid price ema: %f boll band width: %f", spread.Float64(), midPriceEMA, bandWidth)

	n := s.liquidityScale.Sum(1.0)

	var bidPrices []fixedpoint.Value
	var askPrices []fixedpoint.Value

	// calculate and collect prices
	for i := 0; i <= s.NumOfLiquidityLayers; i++ {
		fi := fixedpoint.NewFromInt(int64(i))
		sp := tickSize.Mul(fi)

		bidPrice := ticker.Buy
		askPrice := ticker.Sell

		if i == s.NumOfLiquidityLayers {
			bwf := fixedpoint.NewFromFloat(bandWidth)
			bidPrice = midPrice.Add(bwf.Neg())
			askPrice = midPrice.Add(bwf)
		} else if i > 0 {
			bidPrice = midPrice.Sub(sp)
			askPrice = midPrice.Add(sp)
		}

		if i > 0 && bidPrice.Compare(ticker.Buy) > 0 {
			bidPrice = ticker.Buy.Sub(sp)
		}

		if i > 0 && askPrice.Compare(ticker.Sell) < 0 {
			askPrice = ticker.Sell.Add(sp)
		}

		bidPrice = s.Market.TruncatePrice(bidPrice)
		askPrice = s.Market.TruncatePrice(askPrice)

		bidPrices = append(bidPrices, bidPrice)
		askPrices = append(askPrices, askPrice)
	}

	availableBase := baseBal.Available
	availableQuote := quoteBal.Available

	// check max exposure
	if s.MaxExposure.Sign() > 0 {
		availableQuote = fixedpoint.Min(availableQuote, s.MaxExposure)

		baseQuoteValue := availableBase.Mul(ticker.Sell)
		if baseQuoteValue.Compare(s.MaxExposure) > 0 {
			availableBase = s.MaxExposure.Div(ticker.Sell)
		}
	}

	makerQuota := &bbgo.QuotaTransaction{}
	makerQuota.QuoteAsset.Add(availableQuote)
	makerQuota.BaseAsset.Add(availableBase)

	log.Infof("balances before liq orders: %s, %s",
		baseBal.String(),
		quoteBal.String())

	if !s.Position.IsDust() {
		if s.Position.IsLong() {
			availableBase = availableBase.Sub(s.Position.Base)
			availableBase = s.Market.RoundDownQuantityByPrecision(availableBase)
		} else if s.Position.IsShort() {
			posSizeInQuote := s.Position.Base.Mul(ticker.Sell)
			availableQuote = availableQuote.Sub(posSizeInQuote)
		}
	}

	askX := availableBase.Float64() / n
	bidX := availableQuote.Float64() / (n * (fixedpoint.Sum(bidPrices).Float64()))

	askX = math.Trunc(askX*1e8) / 1e8
	bidX = math.Trunc(bidX*1e8) / 1e8

	var liqOrders []types.SubmitOrder
	for i := 0; i <= s.NumOfLiquidityLayers; i++ {
		bidQuantity := fixedpoint.NewFromFloat(s.liquidityScale.Call(float64(i)) * bidX)
		askQuantity := fixedpoint.NewFromFloat(s.liquidityScale.Call(float64(i)) * askX)
		bidPrice := bidPrices[i]
		askPrice := askPrices[i]

		log.Infof("liqudity layer #%d %f/%f = %f/%f", i, askPrice.Float64(), bidPrice.Float64(), askQuantity.Float64(), bidQuantity.Float64())

		placeBuy := true
		placeSell := true
		averageCost := s.Position.AverageCost
		// when long position, do not place sell orders below the average cost
		if !s.Position.IsDust() {
			if s.Position.IsLong() && askPrice.Compare(averageCost) < 0 {
				placeSell = false
			}

			if s.Position.IsShort() && bidPrice.Compare(averageCost) > 0 {
				placeBuy = false
			}
		}

		quoteQuantity := bidQuantity.Mul(bidPrice)

		if s.Market.IsDustQuantity(bidQuantity, bidPrice) || !makerQuota.QuoteAsset.Lock(quoteQuantity) {
			placeBuy = false
		}

		if s.Market.IsDustQuantity(askQuantity, askPrice) || !makerQuota.BaseAsset.Lock(askQuantity) {
			placeSell = false
		}

		if placeBuy {
			liqOrders = append(liqOrders, types.SubmitOrder{
				Symbol:      s.Symbol,
				Side:        types.SideTypeBuy,
				Type:        types.OrderTypeLimitMaker,
				Quantity:    bidQuantity,
				Price:       bidPrice,
				Market:      s.Market,
				TimeInForce: types.TimeInForceGTC,
			})
		}

		if placeSell {
			liqOrders = append(liqOrders, types.SubmitOrder{
				Symbol:      s.Symbol,
				Side:        types.SideTypeSell,
				Type:        types.OrderTypeLimitMaker,
				Quantity:    askQuantity,
				Price:       askPrice,
				Market:      s.Market,
				TimeInForce: types.TimeInForceGTC,
			})
		}
	}

	makerQuota.Commit()

	createdOrders, err := s.orderExecutor.SubmitOrders(ctx, liqOrders...)
	if logErr(err, "unable to place liquidity orders") {
		return
	}

	s.liquidityOrderBook.Add(createdOrders...)
}

func profitProtectedPrice(side types.SideType, averageCost, price, feeRate, minProfit fixedpoint.Value) fixedpoint.Value {
	switch side {
	case types.SideTypeSell:
		minProfitPrice := averageCost.Add(
			averageCost.Mul(feeRate.Add(minProfit)))
		return fixedpoint.Max(minProfitPrice, price)

	case types.SideTypeBuy:
		minProfitPrice := averageCost.Sub(
			averageCost.Mul(feeRate.Add(minProfit)))
		return fixedpoint.Min(minProfitPrice, price)

	}
	return price
}

func logErr(err error, msgAndArgs ...interface{}) bool {
	if err == nil {
		return false
	}

	if len(msgAndArgs) == 0 {
		log.WithError(err).Error(err.Error())
	} else if len(msgAndArgs) == 1 {
		msg := msgAndArgs[0].(string)
		log.WithError(err).Error(msg)
	} else if len(msgAndArgs) > 1 {
		msg := msgAndArgs[0].(string)
		log.WithError(err).Errorf(msg, msgAndArgs[1:]...)
	}

	return true
}
