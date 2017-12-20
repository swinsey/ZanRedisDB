package node

import (
	"errors"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKVNode_GeoCommand(t *testing.T) {
	ifGeoHashUnitTest = true

	nd, dataDir, stopC := getTestKVNode(t)
	testKey := []byte("default:test:any_places")

	defer os.RemoveAll(dataDir)
	defer nd.Stop()
	defer close(stopC)

	type tStruct struct {
		name       string
		lat        float64
		lon        float64
		dist       float64
		hash       uint64
		hashBase32 string
	}

	tCases := []tStruct{
		tStruct{
			name: "Tian An Men Square",
			lat:  39.905637761392, lon: 116.39763057232,
			dist:       0,
			hash:       4069885364411786,
			hashBase32: "wx4g08w5jm0",
		},
		tStruct{
			name: "The Great Wall",
			lat:  40.359759768836, lon: 116.02002181113,
			dist:       59853.4742,
			hash:       4069895257856587,
			hashBase32: "wx4t8570wk0",
		},
		tStruct{
			name: "The Palace Museum",
			lat:  39.916345328893, lon: 116.39715582132,
			dist:       1191.8406,
			hash:       4069885548623625,
			hashBase32: "wx4g0dtcd60",
		},
		tStruct{
			name: "The Summer Palace",
			lat:  39.999886103047, lon: 116.27552270889,
			dist:       14774.6742,
			hash:       4069880322548821,
			hashBase32: "wx4etcv20p0",
		},
		tStruct{
			name: "Great Hall of the people",
			lat:  39.9050003, lon: 116.3939423,
			dist:       322.7538,
			hash:       4069885362257819,
			hashBase32: "wx4g087rrr0",
		},
		tStruct{
			name: "Terracotta Warriors and Horses",
			lat:  34.384972, lon: 109.274127,
			dist:       880281.2654,
			hash:       4040142446455543,
			hashBase32: "wqjewedfzx0",
		},
		tStruct{
			name: "West Lake",
			lat:  30.150197, lon: 120.094491,
			dist:       1135799.4856,
			hash:       4054121678641499,
			hashBase32: "wtm7sbdjsk0",
		},
		tStruct{
			name: "Hainan ends of the earth",
			lon:  109.205175, lat: 18.173128,
			dist:       2514090.2704,
			hash:       3974157332439237,
			hashBase32: "w7jxmh2f1h0",
		},
		tStruct{
			name: "Pearl of the Orient",
			lon:  121.49491, lat: 31.24169,
			dist:       1067807.3858,
			hash:       4054803515096369,
			hashBase32: "wtw3sxmuh80",
		},
		tStruct{
			name: "Buckingham Palace",
			lon:  -0.83279, lat: 51.30387,
			dist:       8193510.0282,
			hash:       2163507521029941,
			hashBase32: "gcp7v59ddw0",
		},
		tStruct{
			name: "Taj Mahal",
			lon:  78.23188, lat: 27.102839,
			dist:       3780302.7628,
			hash:       3631332645702463,
			hashBase32: "tszdhjytqz0",
		},
		tStruct{
			name: "Sydney Opera House, Australia",
			lon:  151.12541, lat: -33.512513,
			dist:       8912296.5074,
			hash:       3252040564825549,
			hashBase32: "r653qgnpmq0",
		},
		tStruct{
			name: "Pyramids, Egypt",
			lon:  31.8506, lat: 29.584341,
			dist:       7525469.5594,
			hash:       3491552924055853,
			hashBase32: "stq8kc8vkb0",
		},
		tStruct{
			name: "Statue of Liberty, New York City, USA",
			lon:  -74.24038, lat: 40.412148,
			dist:       11022442.0136,
			hash:       1791816099668153,
			hashBase32: "dr5jysgccf0",
		},
		tStruct{
			name: "Mount verest",
			lon:  86.9221941736, lat: 27.9782502279,
			dist:       3007044.9039,
			hash:       3639839274149119,
			hashBase32: "tuvz1vqc1g0",
		},
	}

	/* Test geoadd. */
	testCmd := "geoadd"
	cmdArgs := make([][]byte, len(tCases)*3+2)
	cmdArgs[0] = []byte(testCmd)
	cmdArgs[1] = testKey

	for i, j := 0, 2; i < len(tCases); i++ {
		cmdArgs[j] = []byte(strconv.FormatFloat(tCases[i].lon, 'g', -1, 64))
		cmdArgs[j+1] = []byte(strconv.FormatFloat(tCases[i].lat, 'g', -1, 64))
		cmdArgs[j+2] = []byte(tCases[i].name)
		j = j + 3
	}

	handlerCmd := buildCommand(cmdArgs)
	c := &fakeRedisConn{}
	handler, _, _ := nd.router.GetCmdHandler(testCmd)
	handler(c, handlerCmd)
	assert.Nil(t, c.GetError())
	c.Reset()

	/* Test geohash. */
	testCmd = "geohash"
	cmdArgs[0] = []byte(testCmd)
	cmdArgs[1] = testKey
	for i := 0; i < len(tCases); i++ {
		cmdArgs[i+2] = []byte(tCases[i].name)
	}
	cmdArgs = cmdArgs[:len(tCases)+2]

	handlerCmd = buildCommand(cmdArgs)
	handler, _, _ = nd.router.GetCmdHandler(testCmd)
	handler(c, handlerCmd)

	assert.Equal(t, len(tCases), c.rsp[0],
		"response array length of geohash mismatch")

	for i, tCase := range tCases {
		if hb, ok := c.rsp[i+1].([]byte); !ok {
			t.Fatalf("response of command:geohash should in type of []byte, %v", c.rsp[i])
		} else if string(hb) != tCase.hashBase32 {
			t.Fatalf("GeoHash of %s should be [%s] not [%s]", tCase.name, tCase.hashBase32, string(hb))
		}
	}
	c.Reset()

	/* Test geodist. */
	testCmd = "geodist"
	center := []byte("Tian An Men Square")
	cmdArgs[0] = []byte(testCmd)
	cmdArgs[1] = testKey
	cmdArgs[2] = center
	unitMap := map[string]float64{
		"m":  1.0,
		"km": 1000.0,
		"ft": 0.3048,
		"mi": 1609.34,
	}

	for unit, toMeters := range unitMap {
		for _, tCase := range tCases {
			cmdArgs[3] = []byte(tCase.name)
			cmdArgs[4] = []byte(unit)
			cmdArgs = cmdArgs[:5]

			handlerCmd = buildCommand(cmdArgs)
			handler, _, _ = nd.router.GetCmdHandler(testCmd)
			handler(c, handlerCmd)

			assert.Nil(t, c.GetError(), "test command: geodist failed")

			if b, ok := c.rsp[0].([]byte); !ok {
				t.Fatalf("response of command:geodist should in type of []byte, %v", c.rsp[0])
			} else {
				if dist, err := strconv.ParseFloat(string(b), 64); err != nil {
					t.Fatalf("parse response of command:geodist failed, err:%s", err.Error())
				} else if math.Abs(dist-(tCase.dist/toMeters)) > 0.5 {
					t.Fatalf("distance between %s and %s is %f%s, not %f%s",
						string(center), tCase.name, (tCase.dist / toMeters), unit, dist, unit)
				}
			}

			c.Reset()
		}
	}

	c.Reset()

	/* Test geopos. */
	testCmd = "geopos"
	cmdArgs[0] = []byte(testCmd)
	cmdArgs[1] = testKey
	cmdArgs = cmdArgs[:len(tCases)+2]
	for i, tCase := range tCases {
		cmdArgs[i+2] = []byte(tCase.name)
	}

	handlerCmd = buildCommand(cmdArgs)
	handler, _, _ = nd.router.GetCmdHandler(testCmd)
	handler(c, handlerCmd)
	assert.Nil(t, c.GetError(), "test command: geopos failed")

	assert.Equal(t, len(tCases), c.rsp[0],
		"total response length from geopos mismatch")

	for i, tCase := range tCases {
		assert.Equal(t, 2, c.rsp[3*i+1])
		/* Check the longitude of the position */
		if ok, err := convIBytes2Float64AndCompare(c.rsp[3*i+2], tCase.lon, 0.0001); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "longitude of %s should be %f±0.0001", tCase.name, tCase.lon)
		}

		/* Check the latitude of the position */
		if ok, err := convIBytes2Float64AndCompare(c.rsp[3*i+3], tCase.lat, 0.0001); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "latitude of %s should be %f±0.0001", tCase.name, tCase.lat)
		}
	}
	c.Reset()

	/* Test  georadius, ASC order. */
	testCmd = "georadiusbymember"
	cmdArgs[0] = []byte(testCmd)
	cmdArgs[1] = testKey
	cmdArgs[2] = center
	cmdArgs[3] = []byte("88282")
	cmdArgs[4] = []byte("m")
	cmdArgs[5] = []byte("withcoord")
	cmdArgs[6] = []byte("withdist")
	cmdArgs[7] = []byte("withhash")
	cmdArgs[8] = []byte("count")
	cmdArgs[9] = []byte("3")
	cmdArgs[10] = []byte("ASC")
	cmdArgs = cmdArgs[:11]

	handlerCmd = buildCommand(cmdArgs)
	handler, _, _ = nd.router.GetCmdHandler(testCmd)
	handler(c, handlerCmd)

	sortedResult := []tStruct{
		tStruct{
			name: "Tian An Men Square",
			lat:  39.905637761392, lon: 116.39763057232,
			dist:       0,
			hash:       4069885364411786,
			hashBase32: "wx4g08w5jm0",
		},
		tStruct{
			name: "Great Hall of the people",
			lat:  39.9050003, lon: 116.3939423,
			dist:       322.7538,
			hash:       4069885362257819,
			hashBase32: "wx4g087rrr0",
		},
		tStruct{
			name: "The Palace Museum",
			lat:  39.916345328893, lon: 116.39715582132,
			dist:       1191.8406,
			hash:       4069885548623625,
			hashBase32: "wx4g0dtcd60",
		},
		tStruct{
			name: "The Summer Palace",
			lat:  39.999886103047, lon: 116.27552270889,
			dist:       14774.6742,
			hash:       4069880322548821,
			hashBase32: "wx4etcv20p0",
		},
		tStruct{
			name: "The Great Wall",
			lat:  40.359759768836, lon: 116.02002181113,
			dist:       59853.4742,
			hash:       4069895257856587,
			hashBase32: "wx4t8570wk0",
		},
	}

	assert.Nil(t, c.GetError(), "test command: georadiusbymember failed")
	//assert.Equal(t, len(sortedResult)*7+1, len(c.rsp))
	assert.Equal(t, 3*7+1, len(c.rsp))
	assert.Equal(t, 3, c.rsp[0])

	c.rsp = c.rsp[1:]
	for i := 0; i < 3; i++ {
		assert.Equal(t, 4, c.rsp[i*7])
		assert.Equal(t, []byte(sortedResult[i].name), c.rsp[i*7+1])

		if ok, err := convIBytes2Float64AndCompare(c.rsp[i*7+2],
			sortedResult[i].dist, 0.5); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "distance between %s and %s should be %f±0.5m", string(center),
				sortedResult[i].name, sortedResult[i].dist)
		}

		assert.Equal(t, []byte(strconv.FormatUint(sortedResult[i].hash, 10)), c.rsp[i*7+3])
		assert.Equal(t, 2, c.rsp[i*7+4])

		if ok, err := convIBytes2Float64AndCompare(c.rsp[i*7+5],
			sortedResult[i].lon, 0.0001); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "longitude of %s should be %f±0.0001",
				sortedResult[i].name, sortedResult[i].lon)
		}

		if ok, err := convIBytes2Float64AndCompare(c.rsp[i*7+6],
			sortedResult[i].lat, 0.0001); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "latitude of %s should be %f±0.0001",
				sortedResult[i].name, sortedResult[i].lat)
		}
	}

	// Test desc order.
	c.Reset()
	cmdArgs[10] = []byte("DESC")
	handlerCmd = buildCommand(cmdArgs)
	handler, _, _ = nd.router.GetCmdHandler(testCmd)
	handler(c, handlerCmd)

	assert.Nil(t, c.GetError(), "test command: georadiusbymember desc failed")
	assert.Equal(t, 3*7+1, len(c.rsp))
	assert.Equal(t, 3, c.rsp[0])

	c.rsp = c.rsp[1:]
	sortedResult = sortedResult[3:]
	for i, j := len(sortedResult)-1, 0; i >= 0; i, j = i-1, j+1 {
		assert.Equal(t, 4, c.rsp[j*7])
		assert.Equal(t, []byte(sortedResult[i].name), c.rsp[j*7+1])

		if ok, err := convIBytes2Float64AndCompare(c.rsp[j*7+2],
			sortedResult[i].dist, 0.5); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "distance between %s and %s should be %f±0.5m", string(center),
				sortedResult[i].name, sortedResult[i].dist)
		}

		assert.Equal(t, []byte(strconv.FormatUint(sortedResult[i].hash, 10)), c.rsp[j*7+3])
		assert.Equal(t, 2, c.rsp[j*7+4])

		if ok, err := convIBytes2Float64AndCompare(c.rsp[j*7+5],
			sortedResult[i].lon, 0.0001); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "longitude of %s should be %f±0.0001",
				sortedResult[i].name, sortedResult[i].lon)
		}

		if ok, err := convIBytes2Float64AndCompare(c.rsp[j*7+6],
			sortedResult[i].lat, 0.0001); err != nil {
			t.Fatal(err)
		} else {
			assert.True(t, ok, "latitude of %s should be %f±0.0001",
				sortedResult[i].name, sortedResult[i].lat)
		}
	}
}

func convIBytes2Float64AndCompare(i interface{}, v, deviation float64) (bool, error) {
	buf, ok := i.([]byte)
	if !ok {
		return false, errors.New("interface's type is not []byte")
	}
	if fvalue, err := strconv.ParseFloat(string(buf), 64); err != nil {
		return false, err
	} else if math.Abs(fvalue-v) > deviation {
		return false, nil
	} else {
		return true, nil
	}
}