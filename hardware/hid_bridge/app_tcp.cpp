#include "app_tcp.h"
#include "ble_link.h"
#include "netcfg.h"
#include "logbuf.h"
#include "src/cli/cli_proto.h"
#include "lwip/sockets.h"
#include <esp_random.h>

namespace {
constexpr size_t CAP = 1100;
static SemaphoreHandle_t lock;
static int client = -1;
static bool authenticated;
static uint8_t sendKey[16];
static uint32_t sendCounter, workedAt;

bool readAll(int fd, uint8_t *p, size_t n) {
  while (n) { int k=recv(fd,p,n,0); if(k<=0)return false; p+=k;n-=k; }
  return true;
}
bool writeAll(int fd, const uint8_t *p, size_t n) {
  while(n) { int k=send(fd,p,n,0);if(k<=0)return false;p+=k;n-=k; }
  return true;
}
bool record(int fd,const uint8_t *p,size_t n) {
  uint8_t hdr[2]={(uint8_t)n,(uint8_t)(n>>8)};
  return writeAll(fd,hdr,2)&&writeAll(fd,p,n);
}
size_t readRecord(int fd,uint8_t *p) {
  uint8_t h[2];if(!readAll(fd,h,2))return 0;
  size_t n=h[0]|h[1]<<8;
  return n && n<=CAP && readAll(fd,p,n) ? n : 0;
}
void serve(int fd) {
  timeval timeout={6,0};
  setsockopt(fd,SOL_SOCKET,SO_RCVTIMEO,&timeout,sizeof timeout);
  timeout.tv_sec=2;
  setsockopt(fd,SOL_SOCKET,SO_SNDTIMEO,&timeout,sizeof timeout);
  int one=1;setsockopt(fd,IPPROTO_TCP,TCP_NODELAY,&one,sizeof one);
  uint8_t frame[CAP], plain[CAP], master[32], recvKey[16], cnonce[16], bnonce[16], welcome[33];
  size_t n=readRecord(fd,frame);
  if(n!=17 || frame[0]!=CLI_HELLO || !net_app_key(master))return;
  memcpy(cnonce,frame+1,16);esp_fill_random(bnonce,16);
  uint8_t txKey[16];
  if(!cli_session(master,cnonce,bnonce,recvKey,txKey))return;
  welcome[0]=CLI_WELCOME;memcpy(welcome+1,bnonce,16);
  cli_mac(master,"app-welcome-v1",cnonce,16,bnonce,16,welcome+17);
  memset(master,0,sizeof master);
  if(!record(fd,welcome,sizeof welcome))return;
  uint32_t received=0;
  while((n=readRecord(fd,frame))) {
    uint32_t counter;
    int len=app_open(recvKey,'C',frame,n,&counter,plain);
    if(len<2 || received==UINT32_MAX || counter!=received+1 || plain[0]!='c' || memchr(plain+1,0,len-1))break;
    received=counter;
    if(!authenticated) {
      xSemaphoreTake(lock,portMAX_DELAY);
      client=fd;memcpy(sendKey,txKey,16);sendCounter=0;authenticated=true;workedAt=0;
      xSemaphoreGive(lock);
      board_log("app: encrypted Wi-Fi session up");
    }
    String line((const char*)plain+1,len-1);
    int sp=line.indexOf(' ');
    String command=sp<0 ? "" : line.substring(sp+1);
    if(command!="ping" && !command.startsWith("log ") && command!="move 1 0" && command!="move -1 0") {
      xSemaphoreTake(lock,portMAX_DELAY);workedAt=millis()|1;xSemaphoreGive(lock);
    }
    ble_push_cmd(FROM_APP,line.c_str(),line.length());
  }
  xSemaphoreTake(lock,portMAX_DELAY);
  authenticated=false;client=-1;workedAt=0;memset(sendKey,0,sizeof sendKey);
  xSemaphoreGive(lock);
  board_log("app: Wi-Fi session down");
}
void task(void*) {
  int listener=socket(AF_INET,SOCK_STREAM,0);
  int one=1;setsockopt(listener,SOL_SOCKET,SO_REUSEADDR,&one,sizeof one);
  sockaddr_in address={};address.sin_family=AF_INET;address.sin_port=htons(7203);address.sin_addr.s_addr=htonl(INADDR_ANY);
  if(listener<0 || bind(listener,(sockaddr*)&address,sizeof address)<0 || listen(listener,1)<0) {
    if(listener>=0)close(listener);board_log("app: network listener failed");vTaskDelete(NULL);return;
  }
  for(;;) { int fd=accept(listener,NULL,NULL);if(fd>=0){serve(fd);close(fd);} }
}
}

bool app_tcp_begin() {
  lock=xSemaphoreCreateMutex();
  return lock && xTaskCreatePinnedToCore(task,"app_tcp",8192,NULL,3,NULL,1)==pdPASS;
}
bool app_tcp_connected() {
  if(!lock)return false;
  xSemaphoreTake(lock,portMAX_DELAY);bool on=authenticated;xSemaphoreGive(lock);return on;
}
bool app_tcp_active() {
  if(!lock)return false;
  xSemaphoreTake(lock,portMAX_DELAY);
  bool active=authenticated && workedAt && millis()-workedAt<10000;
  xSemaphoreGive(lock);return active;
}
bool app_tcp_send(char kind,const String &text) {
  if(!lock || text.length()+1+APP_OVERHEAD>CAP)return false;
  xSemaphoreTake(lock,portMAX_DELAY);
  static uint8_t plain[CAP],frame[CAP];bool ok=false;
  if(authenticated && sendCounter<UINT32_MAX) {
    plain[0]=kind;memcpy(plain+1,text.c_str(),text.length());
    size_t n=app_seal(sendKey,'B',++sendCounter,plain,text.length()+1,frame);
    ok=n && record(client,frame,n);
    if(!ok)shutdown(client,SHUT_RDWR);
  }
  xSemaphoreGive(lock);return ok;
}
