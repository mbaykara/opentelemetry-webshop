package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jinzhu/gorm"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

type Order struct {
	ID     uint   `json:"id" gorm:"primary_key"`
	Item   string `json:"item"`
	Amount int    `json:"amount"`
	Paid   bool   `json:"paid"`
}

var db *gorm.DB

const PORT = "8090"

func initTracer() (*sdktrace.TracerProvider, error) {
	otlp_endpoint := os.Getenv("OTLP_ENDPOINT")
	if otlp_endpoint == "" {
		log.Println("OTLP_ENDPOINT is not set, using tempo")
		otlp_endpoint = "tempo:4317"
	}
	exporter, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithEndpoint(otlp_endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("order-service"),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp, nil
}

func main() {
	var err error
	os.Setenv("PORT", PORT)
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dbHost := os.Getenv("DB_HOST")
	db, err = gorm.Open("mysql", dbUser+":"+dbPassword+"@tcp("+dbHost+":3306)/"+dbName+"?parseTime=True")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	db.AutoMigrate(&Order{})

	tp, err := initTracer()
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			panic(err)
		}
	}()

	router := gin.Default()
	router.Use(func(c *gin.Context) {
		ctx := c.Request.Context()
		traceID := trace.SpanFromContext(ctx).SpanContext().TraceID()
		c.Set("traceID", traceID.String())
		c.Next()
	}, gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("clientIP: %s Method: %s Path: %s Req: %s StatusCode %d Latency: %s Agent: %s traceID: %s\"\n",
			param.ClientIP,
			param.Method,
			param.Path,
			param.Request.Proto,
			param.StatusCode,
			param.Latency,
			param.Request.UserAgent(),
			param.Keys["traceID"].(string),
		)
	}))
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.POST("/orders", createOrder)
	router.GET("/orders/:id", getOrder)
	router.PUT("/orders/:id", updateOrder)
	router.PUT("/orders/:id/pay", payOrder)
	router.GET("/orders/:id/payment", getPayment)

	if err := router.Run(); err != nil {
		panic(err)
	}
}

func createOrder(c *gin.Context) {
	tracer := otel.Tracer("order-service")
	var order Order
	if err := c.BindJSON(&order); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, span := tracer.Start(c.Request.Context(), "Create Order")
	defer span.End()

	// Create order in DB
	if err := createOrderWithContext(ctx, &order); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	span.SetAttributes(
		attribute.String("item", order.Item),
		attribute.Int("amount", order.Amount),
	)

	c.JSON(http.StatusCreated, order)
}

func createOrderWithContext(ctx context.Context, order *Order) error {

	tracer := otel.Tracer("order-service")
	_, span := tracer.Start(ctx, "trace: create order in db")
	defer span.End()

	if err := db.Create(order).Error; err != nil {
		span.SetStatus(codes.Error, "Failed to create order in database")
		return err
	}

	return nil
}

func getOrder(c *gin.Context) {
	_, span := otel.Tracer("order-service").Start(c.Request.Context(), "getOrder")
	defer span.End()

	var order Order
	id := c.Param("id")

	if err := db.First(&order, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		span.RecordError(fmt.Errorf("Order not found"))
		return
	}

	span.SetAttributes(
		attribute.String("order_id", id),
	)

	c.JSON(http.StatusOK, order)
}

func updateOrder(c *gin.Context) {
	_, span := otel.Tracer("order-service").Start(c.Request.Context(), "updateOrder")
	defer span.End()

	var order Order
	id := c.Param("id")

	if err := db.First(&order, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		span.RecordError(fmt.Errorf("Order not found"))
		return
	}

	if err := c.BindJSON(&order); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := db.Save(&order).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		span.RecordError(err)
		return
	}

	span.SetAttributes(
		attribute.String("order_id", id),
	)

	c.JSON(http.StatusOK, order)
}

func payOrder(c *gin.Context) {
	_, span := otel.Tracer("order-service").Start(c.Request.Context(), "payOrder")
	defer span.End()

	var order Order
	id := c.Param("id")

	if err := db.First(&order, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		span.RecordError(fmt.Errorf("no order exists with id %s", id))
		return
	}
	order.Paid = true

	if err := db.Save(&order).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	span.SetAttributes(
		attribute.String("order_id", id),
	)
	c.JSON(http.StatusOK, order)
}

func getPayment(c *gin.Context) {
	ctx, span := otel.Tracer("order-service").Start(c.Request.Context(), "Get Payment")
	defer span.End()

	orderID := c.Param("id")
	paymentServiceURL := os.Getenv("PAYMENT_SERVICE_URL") // Get the URL of your payment service

	if paymentServiceURL == "" {
		span.RecordError(fmt.Errorf("PAYMENT_SERVICE_URL environment variable not set"))
		span.SetStatus(codes.Error, "Missing environment variable")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "PAYMENT_SERVICE_URL not configured"})
		return
	}

	paymentURL := fmt.Sprintf("%s/payments/%s", paymentServiceURL, orderID)
	req, err := http.NewRequestWithContext(ctx, "GET", paymentURL, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to create request")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Instrument the HTTP client for tracing
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "payment service request failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		span.SetStatus(codes.Error, fmt.Sprintf("Payment service returned %d", resp.StatusCode))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get payment from payment service"})
		return
	}

	var paymentData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&paymentData); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Failed to decode payment data")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	span.SetAttributes(
		attribute.String("payment.order_id", orderID),
		attribute.String("payment.status", paymentData["status"].(string)),
	)
	c.JSON(http.StatusOK, paymentData)
}
