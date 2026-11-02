package sitesignin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func TestIncomingCommandUsesOneFrozenScopeAndRepliesOnlyToSender(t *testing.T){
	s,scope,guard:=signinRecordFixture(t);ctx:=context.Background()
	if _,e:=s.db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,SWITCHS TEXT,INTERACTIVE INTEGER,ENABLED INTEGER,NOTE TEXT)`);e!=nil{t.Fatal(e)}
	for _,id:=range []int{7,8}{kind:="telegram";if id==8{kind="wechat"};if _,e:=s.db.Exec(`INSERT INTO MESSAGE_CLIENT VALUES(?, 'fixture-channel', ?, '{"token":"fixture-private"}', '[]',1,1,'')`,id,kind);e!=nil{t.Fatal(e)}}
	channel:=notificationconfig.Channel{ID:7,Name:"fixture-channel",Type:"telegram",Config:`{"token":"fixture-private"}`,Switches:"[]",Interactive:1,Enabled:1}
	source:=sitelogin.Hash(channel);scope.Options.Remote=true;scope.CommandDigest=sitelogin.Hash([]notificationconfig.Channel{channel});control:=Control{Scope:scope,Enabled:true};if e:=s.SetControl(ctx,Control{},control,guard);e!=nil{t.Fatal(e)}
	data:=map[string]string{"channel":"Telegram","user_id":"1234","user_name":"fixture-user","message":"/pts"};raw:=json.RawMessage(`{"update_id":999,"message":{"text":"/pts"}}`)
	for i:=0;i<2;i++{if e:=s.Incoming(ctx,scope,source,"provider-update-999","1234",7,raw,data,true,guard);e!=nil{t.Fatal(e)}}
	signals,e:=s.Signals(ctx,true);if e!=nil||len(signals)!=1||signals[0].Scope.ID!=scope.ID{t.Fatalf("signal binding %+v %v",signals,e)}
	queue:=bus.New(s.db);pending,e:=queue.Pending(ctx);if e!=nil||len(pending)!=1||pending[0].ChannelID!=7{t.Fatalf("acknowledgement broadcast %+v %v",pending,e)}
	event,recipient,e:=queue.Claim(ctx,pending[0]);if e!=nil||recipient.ID!=7||event.Kind!="site_signin_ack"{t.Fatal("original reply target lost",e)};var reply map[string]string;if json.Unmarshal(event.Data,&reply)!=nil||reply["user_id"]!="1234"{t.Fatal("original sender lost")}
	if e=s.StopControl(ctx,scope.ID,"stop");e!=nil{t.Fatal(e)};old,e:=s.Control(ctx);if e!=nil{t.Fatal(e)};newScope:=scope;newScope.ID=sitelogin.Random();if e=s.SetControl(ctx,old,Control{Scope:newScope,Enabled:true},guard);e!=nil{t.Fatal(e)}
	if e=s.Incoming(ctx,newScope,source,"provider-update-999","1234",7,raw,data,true,guard);e!=nil{t.Fatal(e)};signals,e=s.Signals(ctx,true);if e!=nil||len(signals)!=1||signals[0].Scope.ID!=scope.ID{t.Fatal("old input rebound to a new grant")}
}
func TestUnknownSlackConnectIsNotAutomaticallyRecreated(t *testing.T){
	s,scope,guard:=signinRecordFixture(t);ctx:=context.Background();scope.Options.Remote=true;scope.CommandDigest=sitelogin.Hash("fixture-channels");if e:=s.SetControl(ctx,Control{},Control{Scope:scope,Enabled:true},guard);e!=nil{t.Fatal(e)}
	source:=sitelogin.Hash("fixture-source");id,e:=s.RemoteIntent(ctx,scope,source,"slack-connect",guard);if e!=nil{t.Fatal(e)}
	unknown,e:=s.UnconfirmedRemoteConnect(ctx,scope.ID,source);if e!=nil||!unknown{t.Fatal("interrupted connection ignored")}
	if e=s.RemoteReceived(ctx,id,"unknown",0);e!=nil{t.Fatal(e)};unknown,e=s.UnconfirmedRemoteConnect(ctx,scope.ID,source);if e!=nil||!unknown{t.Fatal("unknown connection became replayable")}
	unknown,e=s.UnconfirmedRemoteConnect(ctx,sitelogin.Random(),source);if e!=nil||unknown{t.Fatal("new explicit grant inherited old connection authority")}
}
