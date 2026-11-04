import type { StatsItem, StatsOverview } from "../api/siteStatistics";

export type StatRow=StatsOverview["targets"][number]&{item?:StatsItem};
export type ShareOrder="upload"|"download"|"ratio"|"seedingSize"|"seeding";
export type ShareOptions={order:ShareOrder;direction:"asc"|"desc";hideSite:boolean;hideUser:boolean};
export const shareFields:Record<ShareOrder,string>={
  upload:"上传量",download:"下载量",ratio:"分享率",seedingSize:"做种体积",seeding:"做种数量",
};
export const shareDefaults:ShareOptions={order:"upload",direction:"desc",hideSite:true,hideUser:true};

export function statsSize(value:string|bigint){
  const bytes=BigInt(value);
  const units=["B","KiB","MiB","GiB","TiB","PiB","EiB"];
  let divisor=1n,unit=0;
  while(unit<units.length-1&&bytes>=divisor*1024n){
    divisor*=1024n;
    unit++;
  }
  if(unit===0)return `${bytes} B`;
  const hundredths=(bytes*100n+divisor/2n)/divisor;
  return `${hundredths/100n}.${String(hundredths%100n).padStart(2,"0")} ${units[unit]}`;
}

export function statsRows(data:StatsOverview):StatRow[]{
  const byOrigin=new Map(data.items.map(item=>[item.url,item]));
  const byName=new Map<string,StatsItem|undefined>();
  for(const item of data.items)byName.set(item.site,byName.has(item.site)?undefined:item);
  return data.targets.map(target=>({...target,item:target.url?byOrigin.get(target.url):byName.get(target.name)}));
}

export function statsTotals(rows:readonly StatRow[]){
  let upload=0n,download=0n,seeding=0n,seedingSize=0n;
  const counted=new Set<string>();
  for(const row of rows){
    const item=row.item;
    if(!item||counted.has(item.url))continue;
    counted.add(item.url);
    upload+=BigInt(item.upload);
    download+=BigInt(item.download);
    seeding+=BigInt(item.seeding);
    seedingSize+=BigInt(item.seedingSize);
  }
  return {upload,download,seeding,seedingSize};
}

export function statsRatio(ratio:number,precision=2){
  return ratio===0||ratio>10000?"∞":ratio.toFixed(precision);
}

export function shareCandidates(rows:StatRow[]){
  const seen=new Set<string>();
  return rows.filter((row):row is StatRow&{item:StatsItem}=>{
    if(!row.item||seen.has(row.item.url))return false;
    seen.add(row.item.url);
    return true;
  });
}

export function shareProjection(rows:StatRow[],selected:ReadonlySet<string>,options:ShareOptions){
  const picked=shareCandidates(rows.filter(row=>selected.has(row.id)));
  const value=(item:StatsItem)=>{
    const metric=item[options.order];
    return typeof metric==="string"?BigInt(metric):metric;
  };
  picked.sort((a,b)=>{
    const x=value(a.item),y=value(b.item);
    const cmp=x<y?-1:x>y?1:0;
    return options.direction==="asc"?cmp:-cmp;
  });
  return {
    totals:statsTotals(picked),
    rows:picked.map(({name,host,item},i)=>[
      options.hideSite?`站点 ${i+1}`:[name,host].filter(Boolean).join(" · "),
      options.hideUser?"已隐藏":item.username||"—",
      statsSize(item.upload),statsSize(item.download),statsRatio(item.ratio),
      item.seeding.toLocaleString("zh-CN"),statsSize(item.seedingSize),
    ]),
  };
}

export type ShareProjection=ReturnType<typeof shareProjection>;

export function scatterSample(rows:readonly [string,string][]){
  let maxCount=0n,maxBytes=0n,countIndex=0,bytesIndex=0;
  rows.forEach(([count,bytes],i)=>{
    const people=BigInt(count),size=BigInt(bytes);
    if(people>maxCount){maxCount=people;countIndex=i;}
    if(size>maxBytes){maxBytes=size;bytesIndex=i;}
  });
  const limit=1500;
  if(rows.length<=limit)return {maxCount,maxBytes,points:rows};
  const indices=new Set([0,rows.length-1,countIndex,bytesIndex]);
  for(let i=1;i<limit-1&&indices.size<limit;i++){
    indices.add(Math.round(i*(rows.length-1)/(limit-1)));
  }
  const points=Array.from(indices).sort((a,b)=>a-b).map(i=>rows[i]);
  return {maxCount,maxBytes,points};
}
