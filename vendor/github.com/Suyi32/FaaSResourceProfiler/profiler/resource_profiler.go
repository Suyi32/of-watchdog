package profiler

import (
	"time"
	"io/ioutil"
	"io"
	"log"
	"strings"
	"strconv"
	"os"
	"bufio"
	"fmt"
	"sync"
)

type CPULock struct {
    CPU 	float64
    mux     sync.RWMutex
}

type MemoLock struct {
    Memo 	float64
    mux     sync.RWMutex
}

type NetLock struct {
    RxBytes float64
	TxBytes float64
    mux     sync.RWMutex
}

func (cpulock *CPULock) ReadCPU() float64 {
    cpulock.mux.RLock()          // read lock
    var cpu_res = cpulock.CPU
    cpulock.mux.RUnlock()        // read unlock
    return cpu_res
}

func (memolock *MemoLock) ReadMemo() float64 {
    memolock.mux.RLock()          // read lock
    var memo_res = memolock.Memo
    memolock.mux.RUnlock()        // read unlock
    return memo_res
}

func (netlock *NetLock) ReadNetTxBytes() float64 {
    netlock.mux.RLock()          // read lock
    var net_res = netlock.TxBytes
    netlock.mux.RUnlock()        // read unlock
    return net_res
}

func (netlock *NetLock) ReadNetRxBytes() float64 {
    netlock.mux.RLock()          // read lock
    var net_res = netlock.RxBytes
    netlock.mux.RUnlock()        // read unlock
    return net_res
}

type ResourceProfiler struct {
    lastCPU float64
    Memo float64
	lastTimeStamp float64
	CPUWithLock 	*CPULock
	MemoWithLock 	*MemoLock
	NetWithLock 	*NetLock
}

func (resourceProfiler *ResourceProfiler) ReadCPU() float64 {
	data, err := ioutil.ReadFile("/sys/fs/cgroup/cpuacct/cpuacct.usage")
	if err != nil{
			log.Fatal(err)
	}
	cpu_string := strings.TrimSuffix(string(data), "\n")
	cpu_usage, err := strconv.ParseFloat(cpu_string, 64)
	//fmt.Println(cpu_usage, err, reflect.TypeOf(cpu_usage))
	
	currentCPU := float64(cpu_usage)
	currentTimeStamp := float64(time.Now().UnixNano())

	var res float64 = (currentCPU - resourceProfiler.lastCPU) / (currentTimeStamp - resourceProfiler.lastTimeStamp)

	resourceProfiler.lastTimeStamp = currentTimeStamp
	resourceProfiler.lastCPU = currentCPU

	return res
}

func (resourceProfiler *ResourceProfiler) ReadCPU_daemon() float64 {
	for {
		data, err := ioutil.ReadFile("/sys/fs/cgroup/cpuacct/cpuacct.usage")
		if err != nil{
				log.Fatal(err)
		}
		cpu_string := strings.TrimSuffix(string(data), "\n")
		currentCPU, err := strconv.ParseFloat(cpu_string, 64)
		
		currentTimeStamp := float64(time.Now().UnixNano())
		
		resourceProfiler.CPUWithLock.mux.Lock()
		// resourceProfiler.CPU_ready = (currentCPU - resourceProfiler.lastCPU) / (currentTimeStamp - resourceProfiler.lastTimeStamp)
		resourceProfiler.CPUWithLock.CPU = (currentCPU - resourceProfiler.lastCPU) / (currentTimeStamp - resourceProfiler.lastTimeStamp)
		resourceProfiler.CPUWithLock.mux.Unlock()

		resourceProfiler.lastTimeStamp = currentTimeStamp
		resourceProfiler.lastCPU = currentCPU
		// log.Printf("CPU usage: %f" , resourceProfiler.CPUWithLock.CPU)
		time.Sleep(time.Duration(1) * time.Second)	
	}
}

func (resourceProfiler *ResourceProfiler) ReadNetwork_daemon() float64 {
	file := "/proc/net/dev"
	windowSize := 5
	rxWindow := []float64{}
	txWindow := []float64{}

	f, err := os.Open(file)
	if err != nil{
		log.Fatal(err)
	}
	defer f.Close()

	for {
		_, err = f.Seek(0, io.SeekStart)
		if err != nil {
			log.Fatal(err)
		}
		s := bufio.NewScanner(f)
		
		for n := 0; s.Scan(); n++ {
			// Skip the 2 header lines.
			if n < 2 {
				continue
			}
			rxBytes, txBytes := parseLine(s.Text())
			if (rxBytes == -1 && txBytes == -1) {
				continue
			} 
			// log.Println("rxBytes, txBytes", rxBytes, txBytes)
			if (len(rxWindow) < windowSize && len(txWindow) < windowSize) {
				rxWindow = append(rxWindow, rxBytes)
				txWindow = append(txWindow, txBytes)
			} else {
				rxWindow = append(rxWindow, rxBytes)
				txWindow = append(txWindow, txBytes)
				rxWindow = rxWindow[1:]
				txWindow = txWindow[1:]
			}
			break	
		}
		// log.Println("rxWindow", rxWindow, len(rxWindow), (rxWindow[len(rxWindow)-1] - rxWindow[0]) / float64(len(rxWindow)) )
		// log.Println("txWindow", txWindow, len(txWindow), (txWindow[len(txWindow)-1] - txWindow[0]) / float64(len(txWindow)) )
		
		resourceProfiler.NetWithLock.mux.Lock()
		resourceProfiler.NetWithLock.RxBytes = (rxWindow[len(rxWindow)-1] - rxWindow[0]) / float64(len(rxWindow))
		resourceProfiler.NetWithLock.TxBytes = (txWindow[len(txWindow)-1] - txWindow[0]) / float64(len(txWindow))
		resourceProfiler.NetWithLock.mux.Unlock()

		time.Sleep(time.Duration(1) * time.Second)
	}
}

func parseLine(rawLine string) (float64, float64) {
	parts := strings.SplitN(rawLine, ":", 2)
	if len(parts) != 2 {
		// return nil, errors.New("invalid net/dev line, missing colon")
		return -1, -1
	}
	fields := strings.Fields(strings.TrimSpace(parts[1]))
	devName := strings.TrimSpace(parts[0])

	if devName == "eth0" {
		rxBytes, err := strconv.ParseFloat(fields[0], 64)
		txBytes, err := strconv.ParseFloat(fields[8], 64)
		if err != nil{
			log.Fatal(err)
		}
		return rxBytes, txBytes
	} else {
		return -1, -1
	}
}

func (resourceProfiler *ResourceProfiler) ReadMemo() float64 {
	var memo_usage_in_bytes float64
	var cache float64

	file, err := os.Open("/sys/fs/cgroup/memory/memory.usage_in_bytes")
	if err !=  nil{
			log.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
			_, err := fmt.Sscan(scanner.Text(), &memo_usage_in_bytes)
			//strconv.ParseFloat(f, 64)
			if err != nil{
					log.Fatal(err)
			}
	}
	if err := scanner.Err(); err != nil{
			log.Fatal(err)
	}
	// fmt.Printf("memo=%f, type: %T\n", memo_usage_in_bytes, memo_usage_in_bytes)

	file, err = os.Open("/sys/fs/cgroup/memory/memory.stat")
	if err != nil{
			log.Fatal(err)
	}
	defer file.Close()
	scanner = bufio.NewScanner(file)
	for scanner.Scan() {
		split := strings.Split(scanner.Text(), " ")
		// fmt.Println(split)
		if split[0] == "cache"{
			_, err = fmt.Sscan(split[1], &cache)
		}
	}
	// fmt.Printf("cache=%f, type: %T\n", cache, cache)
	var curMemo float64 = (memo_usage_in_bytes - cache) / 1048576.0
	resourceProfiler.Memo = curMemo
	return curMemo
}

func (resourceProfiler *ResourceProfiler) ReadMemo_daemon() float64 {
	for {
		new_memo := resourceProfiler.ReadMemo()
		
		resourceProfiler.MemoWithLock.mux.Lock()
		resourceProfiler.MemoWithLock.Memo = new_memo 
		resourceProfiler.MemoWithLock.mux.Unlock() 
		// log.Printf("Memo usage: %f" , resourceProfiler.MemoWithLock.Memo)
		time.Sleep(time.Duration(500) * time.Millisecond)
	}
}

func NewResourceProfiler() *ResourceProfiler {
	data, err := ioutil.ReadFile("/sys/fs/cgroup/cpuacct/cpuacct.usage")
	if err != nil{
			log.Fatal(err)
	}
	cpu_string := strings.TrimSuffix(string(data), "\n")
	cpu_usage, err := strconv.ParseFloat(cpu_string, 64)
	
	return &ResourceProfiler{
		lastCPU: float64(cpu_usage),
		Memo: float64(0.0),
		lastTimeStamp: float64(time.Now().UnixNano()),
		CPUWithLock: &CPULock{},
		MemoWithLock: &MemoLock{},
		NetWithLock: &NetLock{},
	}
}