package main

import (
	"log"
	"math"
	"os"
	"reflect"
	"time"

	influxdb "github.com/influxdata/influxdb1-client/v2"
	tesla "github.com/jsgoecke/tesla"
)

const (
	dryRun = false
)

var (
	FIELDS_BLACKLIST = []string{"timestamp", "gps_as_of", "left_temp_direction", "right_temp_direction", "charge_port_latch"}
	INTERVAL         = map[string]int{"driving": 1, "charging_fast": 2, "charging": 16, "asleep": 64}
	batteryRange     = map[string]float64{}
)

type InfluxRowUpdate struct {
	tags   map[string]string
	fields map[string]interface{}
}

type StateTracker struct {
	lastState  string // can be driving, charging, asleep, offline, online
	updateTime time.Time
}

func (tracker *StateTracker) update(v *tesla.Vehicle) string {
	newState := v.State
	if v.State == "online" {
		chargeState, err := v.ChargeState()
		if chargeState != nil {
			switch chargeState.ChargingState {
			case "Charging", "Starting", "Complete", "Stop":
				newState = "charging"
				if chargeState.FastChargerPresent {
					newState = "charging_fast"
				}
			}
		} else {
			log.Print("Can't obtain charge state while vehicle is online", err)
		}
		driveState, err := v.DriveState()
		if driveState != nil {
			shift := driveState.ShiftState
			speed := driveState.Speed
			if shift == "R" || shift == "D" || shift == "N" || speed > 0 {
				newState = "driving"
			}
		} else {
			log.Print("Can't obtain driving state while vehicle is online: ", err)
		}
	}
	oldState := tracker.lastState
	if tracker.lastState != newState {
		tracker.updateTime = time.Now()
		tracker.lastState = newState
	}
	return oldState
}

var (
	teslaClient  *tesla.Client
	dbClient     influxdb.Client
	stateTracker StateTracker
)

func newClient() *tesla.Client {
	client, err := tesla.NewClient(&tesla.Auth{
		ClientID:     os.Getenv("TESLA_CLIENT_ID"),
		ClientSecret: os.Getenv("TESLA_CLIENT_SECRET"),
		Email:        os.Getenv("TESLA_USERNAME"),
		Password:     os.Getenv("TESLA_PASSWORD"),
	})
	if err != nil {
		panic(err)
	}
	return client
}

func newInfluxConn() influxdb.Client {
	client, err := influxdb.NewHTTPClient(influxdb.HTTPConfig{
		Addr:     os.Getenv("INFLUXDB_HOST"),
		Username: os.Getenv("INFLUXDB_USER"),
		Password: os.Getenv("INFLUXDB_PASS"),
	})
	if err != nil {
		panic(err)
	}
	return client
}

func refreshVehicle() tesla.Vehicles {
	vehicles, err := teslaClient.Vehicles()
	if err != nil {
		log.Print(err)
		return nil
	}
	return vehicles
}

// Poll the state into the |state| channel
func pollState(stateChannel chan *tesla.Vehicle) {
	interval := 1
	for {
		log.Printf("Wait for %d seconds for state %s", interval, stateTracker.lastState)
		time.Sleep(time.Duration(interval) * time.Second)
		vehicles := refreshVehicle()
		if vehicles != nil {
			vehicle := vehicles[0].Vehicle
			stateChannel <- vehicle
			interval = updateInterval(vehicle, interval)
		}
	}
}

func createStateUpdate(v *tesla.Vehicle, state interface{}) InfluxRowUpdate {
	return createUpdateInternal(v, createDbFields(state))
}

func createVehicleUpdate(v *tesla.Vehicle) InfluxRowUpdate {
	return createUpdateInternal(v, map[string]interface{}{
		"state": v.State,
	})
}

func createUpdateInternal(v *tesla.Vehicle, fields map[string]interface{}) InfluxRowUpdate {
	tags := map[string]string{
		"vin":          v.Vin,
		"display_name": v.DisplayName,
	}
	return InfluxRowUpdate{tags, fields}
}

func dispatch(in chan *tesla.Vehicle, stateChannels map[string]chan InfluxRowUpdate) {
	for {
		v := <-in
		stateChannels["main"] <- createVehicleUpdate(v)
		if v.State == "asleep" {
			continue
		}
		if vehicle, _ := v.VehicleState(); vehicle != nil {
			stateChannels["vehicle_state"] <- createStateUpdate(v, vehicle)
		}
		if charge, _ := v.ChargeState(); charge != nil {
			stateChannels["charge_state"] <- createStateUpdate(v, charge)
		}
		if drive, _ := v.DriveState(); drive != nil {
			stateChannels["drive_state"] <- createStateUpdate(v, drive)
		}
		if climate, _ := v.ClimateState(); climate != nil {
			stateChannels["climate_state"] <- createStateUpdate(v, climate)
		}
	}
}

func dedup(in chan InfluxRowUpdate, out chan InfluxRowUpdate) {
	var last InfluxRowUpdate
	for {
		update := <-in
		diff := changeDiff(last, update)
		if len(diff.fields) > 0 {
			out <- diff
			last = update
		}
	}
}

func changeDiff(last InfluxRowUpdate, update InfluxRowUpdate) InfluxRowUpdate {
	ret := InfluxRowUpdate{fields: make(map[string]interface{})}
	for k, newValue := range update.fields {
		oldValue, ok := last.fields[k]
		if k == "latitude" || k == "longitude" || !ok {
			ret.fields[k] = update.fields[k]
			continue
		}
		if k == "battery_range" || k == "est_battery_range" || k == "ideal_battery_range" {
			oldFloat, ok := batteryRange[k]
			newFloat, _ := newValue.(float64)
			if !ok || math.Abs(newFloat-oldFloat) > 0.5 {
				ret.fields[k] = update.fields[k]
				batteryRange[k] = newFloat
			}
			continue
		}
		if oldValue != newValue {
			ret.fields[k] = update.fields[k]
		}
	}
	ret.tags = update.tags
	return ret
}

// Return the new interval based on some exponential delay
// If it is driving, poll every 1 sec
// If it is charging, poll every 16 sec or 2 sec based on fast charging state
// If it just becomes online, reset the interval back to 1 sec
// Otherwise, if there are new changes, reduce the interval by half
// Otherwise, double the interval until 2048 sec
func updateInterval(v *tesla.Vehicle, interval int) int {
	newState := v.State
	oldState := stateTracker.update(v)
	if val, ok := INTERVAL[stateTracker.lastState]; ok {
		return val
	}
	if newState == oldState {
		if interval >= 2048 {
			return 2048
		}
		return interval * 2
	}
	// a state change just happened
	if newState == "online" {
		return 1
	}
	log.Printf("Unhandled state change from %s to %s", oldState, newState)
	return interval / 2
}

func createDbFields(i interface{}) map[string]interface{} {
	s := reflect.ValueOf(i).Elem()
	t := reflect.TypeOf(i).Elem()
	if t.Kind() != reflect.Struct {
		panic("Must be a struct")
	}
	m := make(map[string]interface{})
	for i := 0; i < s.NumField(); i++ {
		f := s.Field(i)
		tag := t.Field(i).Tag.Get("json")
		if f.Interface() != nil {
			m[tag] = f.Interface()
		}
	}
	for _, v := range FIELDS_BLACKLIST {
		delete(m, v)
	}
	return m
}

func influxDbWrite(in chan InfluxRowUpdate, measurement string) {
	for {
		update := <-in
		batchPoints, err := influxdb.NewBatchPoints(influxdb.BatchPointsConfig{
			Precision: "s",
			Database:  os.Getenv("INFLUXDB_DBNAME"),
		})
		if err != nil {
			log.Print(err)
			continue
		}
		point, err := influxdb.NewPoint(measurement, update.tags, update.fields, time.Now())
		if err != nil {
			log.Print(err)
			continue
		}
		batchPoints.AddPoint(point)
		if !dryRun {
			log.Println("Writing to influxdb for", measurement)
			err := dbClient.Write(batchPoints)
			if err != nil {
				log.Print(err)
				continue
			}
		} else {
			log.Print(batchPoints)
		}
	}
}

func main() {
	teslaClient = newClient()
	dbClient = newInfluxConn()
	channels := make(map[string]chan InfluxRowUpdate)
	for _, v := range []string{"vehicle_state", "charge_state", "drive_state", "climate_state"} {
		channels[v] = make(chan InfluxRowUpdate)
		dedupChannel := make(chan InfluxRowUpdate)
		go influxDbWrite(dedupChannel, v)
		go dedup(channels[v], dedupChannel)
	}
	channels["main"] = make(chan InfluxRowUpdate)
	go influxDbWrite(channels["main"], "vehicle_state")
	pollQueue := make(chan *tesla.Vehicle)
	go dispatch(pollQueue, channels)
	pollState(pollQueue)
}
