import { statsSize, type ShareProjection } from "./statisticsView";

export const maxStatisticsPngRows=200;

// Draw only the selected display text. This keeps masked fields and remote
// images outside the canvas and the resulting PNG.
export function drawStatisticsPng(canvas:HTMLCanvasElement,report:ShareProjection){
  if(report.rows.length===0||report.rows.length>maxStatisticsPngRows)throw new Error("请选择 1 至 200 个有数据的站点");
  const width=1080,rowHeight=40,firstRow=185;
  canvas.width=width;
  canvas.height=firstRow+report.rows.length*rowHeight+35;
  const ctx=canvas.getContext("2d");
  if(!ctx)throw new Error("当前浏览器无法绘制统计图片");
  ctx.textBaseline="middle";
  ctx.fillStyle="#f1f5f9";
  ctx.fillRect(0,0,width,canvas.height);
  ctx.fillStyle="#17233a";
  ctx.font="bold 32px sans-serif";
  ctx.fillText("NAS Tools · 站点统计",26,38);
  const totals=[
    ["总上传量",statsSize(report.totals.upload)],["总下载量",statsSize(report.totals.download)],
    ["总做种数",report.totals.seeding.toLocaleString("zh-CN")],["总做种体积",statsSize(report.totals.seedingSize)],
  ];
  for(const [i,[label,value]] of totals.entries()){
    const x=26+i*259;
    ctx.fillStyle="#ffffff";
    ctx.fillRect(x,70,251,66);
    ctx.fillStyle="#475569";
    ctx.font="14px sans-serif";
    ctx.fillText(label,x+12,90);
    ctx.fillStyle="#17233a";
    ctx.font="bold 21px sans-serif";
    ctx.fillText(value,x+12,115);
  }
  const columns=[
    {label:"站点",width:245},{label:"用户",width:155},{label:"上传量",width:130},
    {label:"下载量",width:130},{label:"分享率",width:95},{label:"做种数",width:115},{label:"做种体积",width:158},
  ];
  const drawRow=(values:readonly string[],y:number,header=false)=>{
    ctx.fillStyle=header?"#21365b":(y-firstRow)/rowHeight%2===0?"#ffffff":"#f8fafc";
    ctx.fillRect(26,y,width-52,rowHeight);
    ctx.fillStyle=header?"#ffffff":"#26334b";
    ctx.font=header?"bold 16px sans-serif":"16px sans-serif";
    let x=26;
    columns.forEach((column,i)=>{
      const value=values[i].replace(/[\p{Cc}\p{Cf}]/gu," ");
      ctx.fillText(fitted(ctx,value,column.width-24),x+12,y+rowHeight/2);
      x+=column.width;
    });
  };
  drawRow(columns.map(c=>c.label),firstRow-rowHeight,true);
  report.rows.forEach((row,i)=>drawRow(row,firstRow+i*rowHeight));
}

function fitted(ctx:CanvasRenderingContext2D,value:string,width:number){
  if(ctx.measureText(value).width<=width)return value;
  const chars=Array.from(value);
  let left=0,right=chars.length;
  while(left<right){
    const middle=Math.ceil((left+right)/2);
    if(ctx.measureText(chars.slice(0,middle).join("")+"…").width<=width)left=middle;
    else right=middle-1;
  }
  return chars.slice(0,left).join("")+"…";
}
